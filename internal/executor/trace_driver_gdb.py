# Step-through trace driver for a COMPILED language (B.2), run by gdb --batch.
#
# It is the native-language sibling of trace_harness.py: same wire format
# (dalivim-trace-json@2), same bounds discipline, same hostility assumption —
# only the mechanism differs. Python has sys.settrace; a native binary has a
# debugger, so this drives gdb over a `-debug` build.
#
# EVERYTHING THE DEBUGGER READS IS HOSTILE. A native program can leave a pointer
# dangling, scribble a length field, or simply not have written a local yet, and
# the debugger will hand us whatever bytes are there. So:
#
#   * an UNTYPED pointer is never dereferenced — `rawptr`, a function pointer or
#     a pointer to a scalar is reported as its address and nothing else. We DO
#     read through two kinds of typed pointer, and both under the same discipline:
#       - a container whose layout the language defines (an Odin
#         string/slice/dynamic array is {data,len[,cap]}), after sanity-checking
#         `len`, up to MAX_ITEMS, every element guarded. Without this the student
#         sees `{data: 0x5555…, len: 3}` instead of `"ola"` or `[10, 20, 30]` —
#         the implementation, not the value;
#       - a `^T` whose target is an aggregate (struct/array/union), so that a
#         LINKED STRUCTURE draws as boxes and arrows instead of a column of
#         addresses. Expanded breadth-first at the end of the step and bounded by
#         MAX_HEAP_OBJECTS — see _expande_ponteiros, which also states what this
#         costs in a manually-managed language and why it is the honest trade;
#     a dangling or scribbled target yields `opaque` in every case, never a crash;
#   * every value read is wrapped: any gdb error becomes `opaque`, never an
#     exception that kills the driver mid-trace;
#   * a variable is HIDDEN until execution passes its declaring line. DWARF scope
#     covers the whole frame, so before `x := 3` runs, `x` holds stack garbage —
#     showing it teaches the student a lie (measured: n = 140737488344096 before
#     its own initialiser). This is the single most important rule here;
#   * only frames in the STUDENT's file are recorded: `step` otherwise descends
#     into fmt.println and __memset_avx2, and those frames' values are noise read
#     from uninitialised memory;
#   * `context` is dropped — Odin injects it into every frame; it is the
#     compiler's, not the student's.
#
# Bounds mirror the Python harness: steps, heap boxes, container items, fields,
# string length and the total report size are all capped, and ids are remapped to
# a per-trace counter so no real address ever reaches the client. The size cap is
# measured PER STEP and accumulated — measuring the whole accumulated document on
# every step made the cap itself quadratic, which is what put the step limit out
# of reach (see the budget in `collect`).
#
# The heap travels as a DELTA, like the Python harness: the first step with a
# graph carries the whole heap, the ones after carry {set, del}, and a step with
# neither is identical to the previous one. The document announces it with
# `heap_encoding: "delta"` and consumers reassemble by accumulating.
#
# Every cap that can degrade the DRAWING is reported: a step that hit the box cap
# carries `heap_truncated`, and the document rolls it up into `truncated.heap`.
# Without that the student just sees a reference reaching nothing, and reads it as
# a statement about their program instead of about the diagram's limit.
#
# Behaviour is covered by trace_driver_gdb_test.py, which runs THIS file against a
# scripted program through a fake gdb (trace_driver_gdb_fake.py). That proves the
# logic living here — box identity, the delta, return-value attribution, output
# counting, the caps — and proves nothing about Odin's DWARF, which stays the
# on-target smoke's job.
#
# Config arrives by env (never argv): DALIVIM_TRACE_TARGET (the student source
# basename, which is also the file filter), DALIVIM_TRACE_REPORT (where to write),
# DALIVIM_TRACE_MAX_STEPS, DALIVIM_TRACE_MAX_REPORT_BYTES, DALIVIM_TRACE_ENTRY
# (the symbol to break on).

import io
import json
import os
import re

import gdb

TRACE_VERSION = 2
TRACE_FORMAT = "dalivim-trace-json@2"


def _int_env(name, default):
    try:
        v = int(os.environ.get(name, "") or default)
        return v if v > 0 else default
    except (TypeError, ValueError):
        return default


TARGET = os.environ.get("DALIVIM_TRACE_TARGET", "main.odin")
REPORT = os.environ.get("DALIVIM_TRACE_REPORT", "trace.json")
# The traced program's OWN streams. The inferior shares gdb's stdout, so without
# redirecting them the student's output arrives interleaved with the debugger's
# stepping chatter ("Breakpoint 1, main::main () at main.odin:13", every stepped
# source line, "Value returned is $1 = 2"). gdb's `set logging redirect` does NOT
# cover those stop announcements — measured. Redirecting the INFERIOR instead is
# exact: the runner reads these two files back as the student's stdout/stderr and
# drops gdb's stream entirely.
STDOUT_FILE = os.environ.get("DALIVIM_TRACE_STDOUT", "_dalivim_stdout.txt")
STDERR_FILE = os.environ.get("DALIVIM_TRACE_STDERR", "_dalivim_stderr.txt")
# Teto de leitura do stderr do aluno ao procurar a frase do pânico: o arquivo é
# escrito pelo programa dele e pode ter qualquer tamanho.
MAX_STDERR_SCAN = 64 * 1024
ENTRY = os.environ.get("DALIVIM_TRACE_ENTRY", "main::main")
LANGUAGE = os.environ.get("DALIVIM_TRACE_LANGUAGE", "odin")
MAX_STEPS = _int_env("DALIVIM_TRACE_MAX_STEPS", 2500)
MAX_REPORT_BYTES = _int_env("DALIVIM_TRACE_MAX_REPORT_BYTES", 2_000_000)

MAX_GDB_STEPS = MAX_STEPS * 20   # inner budget: stdlib frames we skip over
MAX_HEAP_OBJECTS = 200
MAX_FIELDS = 30
MAX_ITEMS = 30
MAX_STR = 256
MAX_DEPTH = 4
MAX_STACK = 25
# A length field read out of a hostile program is itself untrusted. Anything past
# this is treated as corrupt rather than walked — the cap that keeps a scribbled
# `len` from turning into a multi-gigabyte read.
MAX_SANE_LEN = 1_000_000
# Ponteiros que um passo pode enfileirar para expandir. O teto real de caixas é
# MAX_HEAP_OBJECTS; este só impede a fila de crescer sem limite antes disso.
MAX_PONTEIROS = 400
# Quantos ALVOS de ponteiro um passo expande. Sem este teto o custo é quadrático:
# cada passo reexpande a cadeia inteira, e um programa que constrói uma lista num
# laço vira O(n²). Medido: 20 nós = 1,7s; 40 = 3,4s; 80 = 12s; 150 = timeout.
#
# 32 não é só um número de desempenho, é o mesmo limite de LEGIBILIDADE que já
# conhecíamos: medido antes, 32 caixas geram 62 setas cruzando o canvas, e daí
# para cima o desenho deixa de ensinar. Além do teto os ponteiros restantes
# continuam sendo o endereço — honesto, e é o que era antes desta mudança.
MAX_EXPANDIDOS_POR_PASSO = 32
# E um orçamento para o TRACE INTEIRO. O teto por passo sozinho não basta: o
# custo é passos × caixas, e um laço que constrói 150 nós dá ~450 passos — 450 ×
# 32 leituras ainda estoura o tempo. Sem este orçamento eu transformaria um trace
# que ANTES funcionava (sem desenho, mas funcionava) num timeout, que é entregar
# nada ao aluno. Esgotado, os ponteiros voltam a ser endereços e o passo a passo
# segue inteiro até o fim. 600 mantém o desenho nos primeiros passos (onde a
# estrutura está sendo construída e o aluno está olhando) e devolve o tempo aos
# programas longos: medido, n=150 caiu de 13s para perto do baseline.
MAX_EXPANSOES_NO_TRACE = 600

# Names the compiler injects into every frame; not the student's variables.
IMPLICIT_NAMES = {"context"}


def _clip(s):
    s = str(s)
    return s[:MAX_STR] + "…" if len(s) > MAX_STR else s


def _in_student_file(sal):
    """True when the stop is in the student's own source file."""
    try:
        return sal is not None and sal.symtab is not None and os.path.basename(sal.symtab.filename) == TARGET
    except Exception:
        return False


class Registro:
    """Endereço real → id denso, vivo pelo TRACE inteiro.

    Era por PASSO, e por isso a identidade de uma caixa não atravessava um passo:
    bastava um objeto sair de vista para todos os seguintes serem renumerados, e
    a caixa que o aluno acompanhava trocava de conteúdo debaixo dele. É o mesmo
    conserto que o harness Python recebeu — identidade é a propriedade que o
    diagrama existe para ensinar, e sem ela um diff passo-a-passo compara coisas
    diferentes.

    Contra reuso de endereço: a chave já carrega o TIPO junto do endereço (ver as
    chamadas de `id_for`), e o registro é podado ao conjunto vivo a cada passo."""

    def __init__(self):
        self.ids = {}
        self.n = 0

    def id_for(self, key):
        rid = self.ids.get(key)
        if rid is None:
            self.n += 1
            rid = str(self.n)
            self.ids[key] = rid
        return rid

    def podar(self, vivos):
        self.ids = {k: v for k, v in self.ids.items() if k in vivos}


class Heap:
    """Heap de UM passo, com ids vindos do registro do trace. O teto de objetos
    continua sendo por passo — senão um programa longo o estouraria acumulando —
    e nenhum endereço real (ASLR) chega ao cliente, como no trace_harness.py."""

    def __init__(self, registro, boxes=None):
        self.registro = registro
        self.boxes = {} if boxes is None else boxes
        self.usados = set()
        self.truncated = False
        # endereço real → id da caixa que mora ali. É o que liga um ponteiro à
        # caixa certa (ver _liga_ponteiros).
        self.por_endereco = {}
        # Ponteiros vistos no passo, guardados com o VALOR do gdb para que a
        # expansão aconteça depois, em largura (ver _expande_ponteiros).
        self.pendentes = []

    def pendura(self, valor):
        if len(self.pendentes) < MAX_PONTEIROS:
            self.pendentes.append(valor)

    def marca_endereco(self, valor, rid):
        addr = _addr_int(valor)
        if addr:
            self.por_endereco[addr] = rid

    def id_for(self, key):
        if key in self.usados:
            return self.registro.ids.get(key)
        if len(self.usados) >= MAX_HEAP_OBJECTS:
            return None
        rid = self.registro.id_for(key)
        if rid is not None:
            self.usados.add(key)
        return rid


def _addr_int(valor):
    """O endereço de um valor como INTEIRO, ou None.

    Existe porque as duas pontas falavam formatos diferentes: a caixa era
    chaveada por `str(v.address)` — que o gdb imprime como
    `(main::Node *) 0x7fff…` — e o ponteiro saía como `0x%x`. Os dois lados
    tinham a mesma informação e nunca casavam, então nenhuma seta era desenhada
    entre dois objetos."""
    try:
        a = valor.address
        return int(a) if a is not None else None
    except Exception:
        return None


# Quanto ainda dá para expandir neste trace (ver MAX_EXPANSOES_NO_TRACE).
_orcamento = {"resta": MAX_EXPANSOES_NO_TRACE}


def _aponta_para_agregado(v):
    """O alvo do ponteiro é uma coisa DESENHÁVEL (struct, arranjo, união)?

    Isto é o filtro que separa `^Node` — que o Python Tutor desenharia como uma
    caixa ligada por seta — de um `rawptr` ou de um ponteiro para função, que não
    têm forma declarada e cujo alvo nunca é lido."""
    try:
        alvo = v.type.strip_typedefs().target().strip_typedefs()
        return alvo.code in (gdb.TYPE_CODE_STRUCT, gdb.TYPE_CODE_ARRAY, gdb.TYPE_CODE_UNION)
    except Exception:
        return False


def _expande_ponteiros(heap, depth_serializa):
    """Dá caixa ao que um ponteiro alcança — é isto que faz uma LISTA LIGADA se
    desenhar em vez de virar uma coluna de endereços.

    Aqui a regra de "nunca desreferenciar um ponteiro cru" É relaxada, e vale
    dizer exatamente até onde:

      * só ponteiro TIPADO para agregado (`^Node`, `^[4]int`). `rawptr`, ponteiro
        para função e ponteiro para escalar continuam sendo só o endereço — sem
        forma declarada, ler seria adivinhar;
      * a leitura passa pelo mesmo `value_v2` de todo o resto, com os mesmos
        tetos de campos, itens e tamanho, e embrulhada: um ponteiro pendurado
        levanta erro no gdb e vira `opaque`, nunca derruba o driver;
      * expansão em LARGURA, com fila. Não há limite de profundidade: uma lista
        de 10 nós desenha os 10, e o que a limita é MAX_HEAP_OBJECTS — o mesmo
        teto de sempre. Recursão com MAX_DEPTH cortaria a lista no 4º nó.

    O que se perde: numa linguagem de memória manual, ler depois de um `free`
    mostra um objeto que não existe mais. É um risco REAL e é a razão de a regra
    existir — mas ele já valia para o `data` de toda fatia e string, que este
    driver sempre leu. E, diferente de uma variável ainda não inicializada (que
    seguimos escondendo, porque nenhuma linha do aluno a produziu), ler memória
    liberada é o que o PRÓPRIO programa faz naquela linha: mostrar o mesmo lixo
    que ele veria é a verdade daquele bug, não uma mentira sobre ele."""
    vistos = set()
    expandidos = 0
    while (
        heap.pendentes
        and len(heap.usados) < MAX_HEAP_OBJECTS
        and expandidos < MAX_EXPANDIDOS_POR_PASSO
        and _orcamento["resta"] > 0
    ):
        v = heap.pendentes.pop(0)
        try:
            addr = int(v)
        except Exception:
            continue
        if not addr or addr in vistos or addr in heap.por_endereco:
            continue
        vistos.add(addr)
        if not _aponta_para_agregado(v):
            continue
        try:
            # depth 0: a fila é que dá a largura, então cada alvo começa do zero
            # e os tetos por caixa continuam valendo.
            depth_serializa(v.dereference(), heap)
            expandidos += 1
            _orcamento["resta"] -= 1
        except Exception:
            continue  # pendurado/ilegível: fica o endereço, que é honesto


def _liga_ponteiros(stack, boxes, por_endereco):
    """Troca `{"prim":"ptr","text":"0x…"}` por `{"ref": id}` quando o endereço é
    de uma caixa que JÁ está no desenho.

    Isto NÃO desreferencia coisa alguma — a regra de nunca ler através de um
    ponteiro cru continua valendo palavra por palavra. É só uma consulta: o
    endereço já foi lido, e a caixa já foi montada a partir de uma variável
    alcançável; ligar as duas é reconhecer uma identidade que estava ali.

    Sem isto, um `prox: ^Node` aparecia como `0x7fffffffe9b8` — o aluno via um
    número onde o Python Tutor desenha uma seta, que é a metade do diagrama que
    ensina estrutura ligada. Um ponteiro para algo que não está no desenho
    (memória de `new` que ninguém mais alcança) continua sendo o endereço."""

    def troca(val):
        if not isinstance(val, dict) or val.get("prim") != "ptr":
            return val
        try:
            addr = int(val.get("text", ""), 16)
        except Exception:
            return val
        rid = por_endereco.get(addr)
        return {"ref": rid} if rid is not None else val

    for frame in stack:
        vars_ = frame.get("vars") or {}
        for nome, val in list(vars_.items()):
            vars_[nome] = troca(val)
    for box in (boxes or {}).values():
        if not isinstance(box, dict):
            continue
        campos = box.get("fields")
        if isinstance(campos, dict):
            for nome, val in list(campos.items()):
                campos[nome] = troca(val)
        itens = box.get("items")
        if isinstance(itens, list):
            box["items"] = [troca(x) for x in itens]
        pares = box.get("entries")
        if isinstance(pares, list):
            box["entries"] = [
                [troca(p[0]), troca(p[1])] if isinstance(p, list) and len(p) == 2 else p
                for p in pares
            ]


def _prim(t, text):
    return {"prim": str(t), "text": _clip(text)}


def _opaque(reason="ilegível"):
    return {"prim": "opaque", "text": "<%s>" % reason}


def _sane_len(v):
    """The `len` field of a container, or None when it is not believable. A
    native program can scribble it, so it is validated before it is used to
    drive any loop."""
    try:
        n = int(v["len"])
    except Exception:
        return None
    return n if 0 <= n <= MAX_SANE_LEN else None


def _odin_string(v):
    """Odin `string`/`cstring` -> the actual text. The layout is {data,len}; we
    read exactly `len` bytes and quote them, so the student sees "ola" instead of
    a pointer and a number."""
    n = _sane_len(v)
    if n is None:
        return _opaque("comprimento inválido")
    try:
        raw = v["data"].string(length=min(n, MAX_STR), errors="replace")
    except Exception:
        return _opaque("texto ilegível")
    text = '"%s"' % raw
    if n > MAX_STR:
        text = text[:-1] + '…"'
    return {"prim": "string", "text": text}


def _ehNulo(ptr):
    try:
        return int(ptr) == 0
    except Exception:
        return False


def _chave(marca, valor, ponteiro, tipo, extra=None):
    """A chave que identifica uma caixa. Normalmente é o PONTEIRO DOS DADOS: duas
    fatias sobre o mesmo arranjo são a mesma coisa, e passar uma para uma proc
    mantém a caixa — que é o comportamento certo.

    Menos em dois casos.

    Quando o ponteiro é NULO ele não identifica nada, e dois `[dynamic]` vazios
    distintos caíam na MESMA caixa: o diagrama desenhava duas setas para um
    objeto só, ensinando que `a` e `b` são aliases quando não são — o oposto
    exato do que ele existe para mostrar. Nesse caso a identidade passa a ser o
    endereço do próprio valor, que distingue as duas variáveis.

    E quando uma SUB-FATIA compartilha o começo do arranjo (`extra`, ver
    _odin_sequence). `a := []int{1,2,3}` com `b := a[:2]` têm o mesmo `data` e o
    mesmo tipo, então caíam na mesma chave — e como a caixa só é montada na
    primeira vez, as duas setas apontavam para uma caixa de TRÊS itens. O aluno
    lia que `b` é a mesma fatia de `a`, contra o `len(b) == 2` do programa dele.
    É a mesma família do bug dos dois vazios, com o ponteiro não nulo."""
    if _ehNulo(ponteiro):
        try:
            propria = valor.address
            if propria is not None:
                return (marca, "vazio@" + str(propria), str(tipo), extra)
        except Exception:
            pass
    return (marca, str(ponteiro), str(tipo), extra)


def _odin_sequence(v, t, heap, depth, kind_name):
    """Odin slice `[]T` / `[dynamic]T` -> a list box with real cells. Both are
    {data,len[,cap]}; `cap` is shown for a dynamic array because "grew to 8 slots
    holding 1" is exactly the thing a student needs to see."""
    n = _sane_len(v)
    if n is None:
        return _opaque("comprimento inválido")
    # O COMPRIMENTO entra na chave de uma FATIA (`[]T`), e só dela. Uma fatia tem
    # cabeçalho imutável — `b := a[:2]` é um valor novo, não uma mutação de `a` —
    # então distinguir por `len` separa a sub-fatia sem custo nenhum.
    #
    # Num `[dynamic]T` seria o contrário: `append` faz o `len` crescer NO LUGAR, e
    # pôr o `len` na chave daria uma caixa nova a cada append — a identidade
    # morreria exatamente onde o diagrama mais precisa dela (ver Registro). E não
    # é preciso: uma sub-fatia de um `[dynamic]` sai como `[]T`, outro tipo, e o
    # tipo já está na chave.
    dinamico = kind_name.startswith("[dynamic]")
    key = _chave("seq", v, v["data"], t, None if dinamico else n)
    rid = heap.id_for(key)
    if rid is not None:
        heap.marca_endereco(v, rid)
    if rid is None:
        heap.truncated = True
        return _opaque("limite de objetos")
    if rid not in heap.boxes:
        box = {"kind": "list", "cls": kind_name, "items": [], "truncated": n > MAX_ITEMS}
        heap.boxes[rid] = box
        items = []
        try:
            data = v["data"]
            for i in range(min(n, MAX_ITEMS)):
                try:
                    items.append(value_v2((data + i).dereference(), heap, depth + 1))
                except Exception:
                    items.append(_opaque())
        except Exception:
            box["truncated"] = True
        box["items"] = items
    return {"ref": rid}


def _odin_map(v, t, heap):
    """Odin `map[K]V`. Its `data` is the runtime's hash table, whose layout is
    internal and version-specific — walking it would be guesswork that breaks on
    the next toolchain bump. So the box is HONEST: the map's type and how many
    entries it holds, explicitly marked partial, instead of invented pairs."""
    rid = heap.id_for(_chave("map", v, v["data"], t))
    if rid is None:
        heap.truncated = True
        return _opaque("limite de objetos")
    if rid not in heap.boxes:
        n = _sane_len(v)
        heap.boxes[rid] = {
            "kind": "object",
            "cls": str(t).replace("struct ", ""),
            "fields": {"len": _prim("int", n if n is not None else "?")},
            "truncated": True,
        }
    return {"ref": rid}


def value_v2(v, heap, depth=0):
    """gdb.Value -> trace v2 value: inline primitive or {"ref": id}. Never raises,
    never dereferences a pointer."""
    try:
        t = v.type.strip_typedefs()
        code = t.code
    except Exception:
        return _opaque()

    try:
        if code in (gdb.TYPE_CODE_INT, gdb.TYPE_CODE_BOOL, gdb.TYPE_CODE_FLT,
                    gdb.TYPE_CODE_ENUM, gdb.TYPE_CODE_CHAR):
            return _prim(t, v)

        if code == gdb.TYPE_CODE_PTR:
            # Address only. Dereferencing a dangling/scribbled pointer is exactly
            # how a hostile program would crash the tracer.
            try:
                n = int(v)
            except Exception:
                return _opaque("ponteiro inválido")
            # `nil` é o que o aluno escreveu; `0x0` é o que a máquina guardou.
            if n == 0:
                return _prim("ptr", "nil")
            # Guarda o VALOR (não só o endereço): a expansão acontece no fim do
            # passo, em largura, para que uma lista ligada inteira se desenhe.
            heap.pendura(v)
            return _prim("ptr", "0x%x" % n)

        if depth >= MAX_DEPTH:
            return _opaque("profundidade")

        # Odin's composite types are STRUCTS at the DWARF level ({data,len}), so
        # they must be recognised by NAME before the generic struct path — that
        # path would faithfully render the implementation and hide the value.
        if code == gdb.TYPE_CODE_STRUCT:
            tname = str(t).replace("struct ", "")
            if tname in ("string", "cstring"):
                return _odin_string(v)
            if tname.startswith("[]"):
                return _odin_sequence(v, t, heap, depth, tname)
            if tname.startswith("[dynamic]"):
                return _odin_sequence(v, t, heap, depth, tname)
            if tname.startswith("map["):
                return _odin_map(v, t, heap)

        if code == gdb.TYPE_CODE_STRUCT:
            key = ("s", _addr_int(v), str(t))
            rid = heap.id_for(key)
            if rid is None:
                heap.truncated = True
                return _opaque("limite de objetos")
            heap.marca_endereco(v, rid)
            if rid not in heap.boxes:
                box = {"kind": "object", "cls": _clip(_nome_curto(str(t).replace("struct ", ""))), "fields": {}, "truncated": False}
                heap.boxes[rid] = box  # inserted BEFORE recursing: cycles terminate
                fields = {}
                for i, f in enumerate(t.fields()):
                    if i >= MAX_FIELDS:
                        box["truncated"] = True
                        break
                    if f.name is None:
                        continue
                    try:
                        fields[f.name] = value_v2(v[f.name], heap, depth + 1)
                    except Exception:
                        fields[f.name] = _opaque()
                box["fields"] = fields
            return {"ref": rid}

        if code == gdb.TYPE_CODE_ARRAY:
            key = ("a", _addr_int(v), str(t))
            rid = heap.id_for(key)
            if rid is None:
                heap.truncated = True
                return _opaque("limite de objetos")
            heap.marca_endereco(v, rid)
            if rid not in heap.boxes:
                box = {"kind": "list", "items": [], "truncated": False}
                heap.boxes[rid] = box
                items = []
                try:
                    low, high = t.range()
                except Exception:
                    low, high = 0, -1
                n = 0
                for i in range(low, high + 1):
                    if n >= MAX_ITEMS:
                        box["truncated"] = True
                        break
                    try:
                        items.append(value_v2(v[i], heap, depth + 1))
                    except Exception:
                        items.append(_opaque())
                    n += 1
                box["items"] = items
            return {"ref": rid}

        return _opaque(t)
    except Exception:
        return _opaque()


def frame_vars(frame, heap, current_line):
    """Locals/args of one frame, EXCLUDING variables execution has not reached
    yet (see the header: DWARF scope != defined).

    Devolve `(vars, no_prologo)`. `no_prologo` diz que existem ARGUMENTOS
    escondidos só porque o prólogo ainda não rodou — o que é diferente de a proc
    não ter parâmetros, e é a diferença que o desenho precisa para não afirmar
    "sem variáveis" no passo em que o aluno acabou de entrar em `dobro(21)`."""
    out = {}
    escondeu_argumento = False
    try:
        block = frame.block()
    except Exception:
        return out, False
    # The function's own declaration line: arguments are only trustworthy once
    # the prologue past it has run.
    try:
        func_line = frame.function().line or 0
    except Exception:
        func_line = 0
    while block is not None:
        try:
            if block.is_global or block.is_static:
                break
            for sym in block:
                if not (sym.is_argument or sym.is_variable):
                    continue
                if sym.name in IMPLICIT_NAMES or sym.name in out:
                    continue
                # STRICTLY greater, not >=: while execution is stopped AT the
                # declaring line, that line has not run yet, so the slot still
                # holds stack garbage. `n := 3` on line 16 must stay hidden at
                # line 16 and appear at line 17. sym.line == 0 means "no line
                # info" — hide rather than show garbage.
                #
                # An ARGUMENT is bound by the caller, but not before the prologue
                # completes: at the function's own declaration line the registers
                # are not yet spilled and gdb reads nonsense (measured: a=1407…
                # at the `soma :: proc(...)` line, a=3 one line later). So the same
                # rule applies, against the FUNCTION's line.
                if sym.is_argument:
                    if current_line <= func_line:
                        escondeu_argumento = True
                        continue
                else:
                    decl = getattr(sym, "line", 0) or 0
                    if decl == 0 or current_line <= decl:
                        continue
                try:
                    out[sym.name] = value_v2(sym.value(frame), heap)
                except Exception:
                    out[sym.name] = _opaque()
            block = block.superblock
        except Exception:
            break
    return out, escondeu_argumento


# Quanto do stdout do aluno já foi contado, e em quantos code points. Incremental
# de propósito: reler o arquivo inteiro a cada passo seria o mesmo erro quadrático
# que o teto de bytes tinha (ver collect).
_saida = {"bytes": 0, "chars": 0, "decoder": None}


def _stdout_ate_agora():
    """Quantos CODE POINTS o programa do aluno já escreveu, para o campo
    `stdout_len` do contrato.

    Antes disto o driver mandava 0 em TODO passo, e o painel "Saída até aqui" do
    passo a passo dizia "(sem saída ainda)" em todos os passos de todo programa
    Odin — inclusive no último, de um programa que imprimiu. O `stdout_len` do
    Python vem de um contador em volta do `sys.stdout`; aqui a fonte é o arquivo
    para onde o inferior foi redirecionado (ver STDOUT_FILE).

    Duas propriedades que fazem isto ser seguro em vez de "plausível e errado":

    * o consumidor FATIA o stdout capturado até este número, então um número
      MENOR que o real mostra um prefixo — texto que o programa de fato escreveu,
      só que menos. Um número maior é que mostraria saída antes da hora, e isso
      não pode acontecer: o tamanho do arquivo nunca passa do que foi escrito;
    * se o `fmt` do Odin bufferizar ao escrever num arquivo (não medido
      on-target), o efeito é exatamente esse atraso, no lado seguro.

    Code points, não bytes: o `stdout_len` do contrato é contado como o
    `Array.from(...).slice(...)` do frontend faz. O decodificador é incremental
    para que um caractere multibyte partido entre duas leituras não vire dois."""
    try:
        import codecs
        if _saida["decoder"] is None:
            _saida["decoder"] = codecs.getincrementaldecoder("utf-8")(errors="replace")
        with io.open(STDOUT_FILE, "rb") as fh:
            fh.seek(_saida["bytes"])
            novos = fh.read()
        if novos:
            _saida["bytes"] += len(novos)
            _saida["chars"] += len(_saida["decoder"].decode(novos))
    except Exception:
        pass  # arquivo ainda não existe, ou ilegível: o contador só não avança
    return _saida["chars"]


def _id_de_quadro(frame):
    """Identidade de um QUADRO VIVO — o que faltava para saber de QUEM é um valor
    devolvido.

    Nem a profundidade da pilha nem a ordem de disparo do breakpoint servem:
    `f(n-1) + f(n-2)` põe duas chamadas na mesma LINHA, o gdb entra e sai da
    primeira sem deixar passo no nível do chamador, e as duas têm a mesma
    profundidade. Foi isso que produziu, nas tentativas anteriores, um quadro com
    `n = 0` dizendo `devolveu 8` — a resposta de `fib(6)`.

    A chave aqui é o ENDEREÇO do quadro, não a posição dele: o par (pc do
    chamador, sp do chamador). O pc do chamador é o endereço de RETORNO, e duas
    chamadas na mesma linha são dois sítios de chamada distintos, logo dois
    endereços distintos. O sp do chamador separa `fib(0)` de `fib(6)`. E o sp do
    PRÓPRIO quadro não serve: ele se move com o prólogo e com pushes.

    Duas chamadas irmãs reusam o mesmo slot de pilha, mas nunca ao mesmo tempo —
    a primeira termina antes de a segunda nascer —, então procurar o passo mais
    recente com esta chave sempre cai dentro do quadro que acabou de sair.

    Devolve None quando não dá para ler (quadro mais externo, gdb sem o
    registrador). Nesse caso a atribuição volta à regra conservadora de sempre."""
    try:
        pai = frame.older()
        if pai is None:
            return None
        return (int(pai.pc()), int(pai.read_register("sp")))
    except Exception:
        return None


def _nome_curto(bruto):
    """`main::soma` → `soma`, `main::Node` → `Node`. O aluno escreveu `soma`, e o prefixo do pacote só
    ocupa espaço numa caixa estreita — o Python Tutor mostra o nome da função,
    não o caminho até ela. Só o último segmento; nomes sem `::` passam intactos."""
    nome = bruto or "?"
    return nome.rsplit("::", 1)[-1] or nome


def snapshot(step_no, registro):
    """One trace v2 step: the student-visible stack, outermost first, plus the
    heap reachable from it."""
    heap = Heap(registro)
    frames = []
    try:
        f = gdb.selected_frame()
    except Exception:
        return None
    while f is not None and len(frames) < MAX_STACK:
        frames.append(f)
        try:
            f = f.older()
        except Exception:
            break

    stack = []
    interno = None
    for fr in reversed(frames):
        try:
            sal = fr.find_sal()
            if not _in_student_file(sal):
                continue
            vars_, no_prologo = frame_vars(fr, heap, sal.line)
            quadro = {
                "func": _nome_curto(fr.name()),
                "file": os.path.basename(sal.symtab.filename),
                "line": sal.line,
                "vars": vars_,
            }
            # Os parâmetros só são legíveis depois do prólogo (ver frame_vars).
            # Sem esta marca o passo de `call` mostra um quadro que o desenho
            # rotula "sem variáveis" — que afirma que a proc não TEM parâmetros,
            # mais forte que "ainda não dá para lê-los".
            if no_prologo:
                quadro["prologo"] = True
            stack.append(quadro)
            interno = fr
        except Exception:
            continue

    try:
        cur = gdb.selected_frame().find_sal()
        line = cur.line
        func = _nome_curto(gdb.selected_frame().name())
    except Exception:
        return None

    # Primeiro dá caixa ao que os ponteiros alcançam (lista ligada), depois liga
    # cada ponteiro à sua caixa. Tem de ser aqui, no fim: quando `a.prox` é
    # serializado, a caixa de `b` pode ainda não ter sido montada.
    _expande_ponteiros(heap, lambda alvo, h: value_v2(alvo, h))
    _liga_ponteiros(stack, heap.boxes, heap.por_endereco)

    # O registro guarda só o que estava vivo NESTE passo: é essa poda que impede
    # um endereço reaproveitado de herdar a caixa de um objeto morto.
    registro.podar(heap.usados)

    passo = {
        "step": step_no,
        # O evento definitivo é decidido em `collect`, comparando a pilha com a
        # do passo anterior: só de lá dá para ver que uma proc foi chamada ou que
        # ela acabou de devolver.
        "event": "line",
        "file": TARGET,
        "line": line,
        "func": func,
        "stdout_len": _stdout_ate_agora(),
        "stack": stack,
        "heap": heap.boxes,
    }
    # O teto de caixas por passo era invisível: a flag era ESCRITA em três lugares
    # e nunca serializada, então o aluno via a variável virar um ponto que não
    # aponta para nada (o frontend não desenha seta para caixa que não existe) sem
    # nenhuma explicação. Agora ela sobe até a faixa "Trace parcial".
    if heap.truncated:
        passo["heap_truncated"] = True
    # A chave do quadro mais interno, para o valor devolvido saber onde morar.
    # Fica FORA do documento (ver collect): é dado de trabalho, não de contrato.
    passo["_quadro"] = _id_de_quadro(interno) if interno is not None else None
    return passo


class _Devolucao(gdb.FinishBreakpoint):
    """Captura o valor que UMA proc devolve.

    É o "Return value" do Python Tutor — a linha `devolveu` no quadro — e era o
    que faltava para o passo a passo do Odin fechar o ciclo mental de uma
    chamada: o aluno via a proc terminar e nunca via o que ela entregou.

    `FinishBreakpoint` é a ferramenta que o gdb tem exatamente para isso: ela se
    arma no endereço de retorno do quadro e expõe `return_value` já com o tipo
    certo, em vez de lermos um registrador na mão e adivinharmos a convenção de
    chamada. `stop()` devolve False de propósito — o ponto é REGISTRAR o valor de
    passagem, nunca interromper o programa do aluno.

    Degradação: uma proc sem tipo de retorno conhecido (ou com retorno múltiplo,
    que o Odin permite) deixa `valor` em None, e o passo sai sem a linha
    `devolveu` — exatamente o comportamento de hoje, nunca um erro."""

    def __init__(self, frame):
        gdb.FinishBreakpoint.__init__(self, frame, internal=True)
        self.silent = True
        self.valor = None
        # DE QUEM é este valor. Ver _id_de_quadro: é o endereço do quadro, e é a
        # única coisa aqui que distingue `f(n-1)` de `f(n-2)`.
        self.quadro = _id_de_quadro(frame)
        # DISPAROU? É este o sinal de que o quadro saiu — e ele é bem mais
        # confiável que comparar a profundidade da pilha entre dois passos.
        # Em `f(n-1) + f(n-2)` as duas chamadas moram na mesma linha: a primeira
        # retorna e a segunda entra sem que exista um passo intermediário no
        # nível do chamador, então a profundidade nunca muda e o retorno passa
        # despercebido — medido em `fibonacci`, todo quadro dizia `devolveu 1`,
        # que era o valor preso da chamada anterior.
        self.saiu = False

    def stop(self):
        try:
            self.valor = self.return_value
        except Exception:
            self.valor = None
        self.saiu = True
        return False

    def out_of_scope(self):
        # O quadro morreu sem passar pelo retorno (sinal, longjmp): sem valor.
        self.valor = None
        self.saiu = True


def _arma_devolucao():
    """Arma a captura para o quadro ATUAL, se der. Falha silenciosa: no quadro
    mais externo não há endereço de retorno, e um gdb sem suporte deve custar o
    valor devolvido, não o trace inteiro."""
    try:
        return _Devolucao(gdb.selected_frame())
    except Exception:
        return None


def _anota_devolvido(passo, bp, registro, base):
    """Prega o valor devolvido no passo de `return`, no heap DAQUELE passo.

    Devolve as caixas NOVAS que isto criou (ou None), para quem chama manter a
    linha de base do delta em dia — o passo já foi codificado quando chegamos
    aqui, então não dá para simplesmente reescrever o heap dele.

    Duas coisas que faltavam e que este passo herdava do resto do desenho:

    * uma proc que devolve `^Node` mostrava `devolveu 0x7fff…` — o NÚMERO onde o
      resto do quadro já desenha seta. `_expande_ponteiros` e `_liga_ponteiros`
      rodam dentro de `snapshot`, e isto acontece depois; então eles precisam
      rodar de novo, sobre o pedaço novo;
    * o `Heap` era construído com as caixas do passo mas com `usados` VAZIO, e é
      `usados` que conta contra MAX_HEAP_OBJECTS. O passo de retorno podia
      duplicar o teto. Agora ele começa sabendo o que já está lá."""
    if bp is None or bp.valor is None:
        return None
    try:
        # `base` é o heap CHEIO daquele passo. Não dá para tirá-lo do próprio
        # passo: quando ele já foi codificado em delta, a chave `heap` não está
        # mais lá — e reconstruir do zero faria este passo remontar caixas que já
        # existem, gastando o teto duas vezes.
        antes = dict(base or {})
        heap = Heap(registro, dict(antes))
        # O teto por passo vale para o passo INTEIRO, retorno incluído: `usados`
        # começa com as chaves que já têm caixa neste passo.
        heap.usados = {k for k, rid in registro.ids.items() if rid in antes}
        passo["retval"] = value_v2(bp.valor, heap)
        # Mesmo tratamento do resto do passo: dar caixa ao que o ponteiro alcança
        # e só então trocar o endereço pela seta. As caixas que já existiam saem
        # daqui intactas — elas já foram ligadas em `snapshot`, e religar uma
        # referência é no-op —, então as caixas NOVAS são exatamente as que este
        # passo acrescentou.
        _expande_ponteiros(heap, lambda alvo, h: value_v2(alvo, h))
        _liga_ponteiros([], heap.boxes, heap.por_endereco)
        retval = passo.get("retval")
        if isinstance(retval, dict) and retval.get("prim") == "ptr":
            try:
                rid = heap.por_endereco.get(int(retval.get("text", ""), 16))
                if rid is not None:
                    passo["retval"] = {"ref": rid}
            except Exception:
                pass
        if heap.truncated:
            passo["heap_truncated"] = True
        # NÃO escreve `passo["heap"]`: o passo já tem a codificação dele (cheia ou
        # delta), e sobrescrever a chave aqui quebraria o delta. Quem chama coloca
        # as caixas novas pela codificação certa — ver _acrescenta_caixas.
        return {rid: box for rid, box in heap.boxes.items() if rid not in antes} or None
    except Exception:
        return None


def _acrescenta_caixas(passo, novas, base):
    """Põe caixas num passo JÁ CODIFICADO, respeitando a codificação dele, e
    atualiza a linha de base para o delta do passo seguinte."""
    if not novas:
        return
    if "heap" in passo:
        passo["heap"].update(novas)
    else:
        passo.setdefault("heap_delta", {}).setdefault("set", {}).update(novas)
    if base is not None:
        base.update(novas)


def _envelope(steps, truncated_steps, crash, heap_truncated=False, bytes_truncated=False):
    """O documento do fio. Existe como função para o orçamento de bytes poder
    medir o envelope antes do primeiro passo, em vez de descobrir no fim que ele
    não cabia."""
    doc = {
        "version": TRACE_VERSION,
        "language": LANGUAGE,
        # O consumidor que não conhecer a flag continua lendo `heap` onde ele
        # aparece; quem conhecer remonta acumulando (parseTrace, heapsOf).
        "heap_encoding": "delta",
        "steps": steps,
        "step_count": len(steps),
        "truncated": {
            "steps": truncated_steps,
            "bytes": bytes_truncated,
            # O teto de caixas por passo. Sem isto ele era invisível: a variável
            # virava um ponto que não aponta para lugar nenhum, sem explicação.
            "heap": heap_truncated,
        },
        "limits": {
            "max_steps": MAX_STEPS,
            "max_report_bytes": MAX_REPORT_BYTES,
            "max_heap_objects": MAX_HEAP_OBJECTS,
        },
    }
    if crash:
        doc["crash"] = crash
    return doc


# Sinais que matam um programa de aluno, com o número (para o código de saída no
# estilo do shell, 128+n) e a frase que o passo a passo mostra. O Odin sinaliza um
# índice fora da faixa com um `ud2`, que chega como SIGILL.
SINAIS = {
    "SIGSEGV": (11, "acesso inválido à memória"),
    "SIGFPE": (8, "erro aritmético (divisão por zero?)"),
    "SIGBUS": (7, "endereço de memória inválido"),
    "SIGILL": (4, "o programa foi interrompido por uma verificação em tempo de execução"),
    "SIGABRT": (6, "o programa abortou"),
}

# Como o programa do ALUNO terminou. O gdb sempre sai 0, então sem isto um
# programa que estourou era classificado como sucesso: o aluno via "execução
# concluída" para um programa que quebrou (medido: `xs[10]` numa fatia de 3 dava
# runtime_error em modo run e success em modo trace).
_fim = {"sinal": None, "codigo": None}


def _ao_sair(evt):
    _fim["codigo"] = getattr(evt, "exit_code", None)


def _ao_parar(evt):
    nome = getattr(evt, "stop_signal", None)
    if nome in SINAIS:
        _fim["sinal"] = nome


def _panico_do_odin():
    """A frase que o PRÓPRIO Odin escreveu ao morrer.

    O sinal responde "como", não "o quê": dizer `SIGILL` a um aluno é o mesmo que
    o Python dizer `SIGFPE` em vez de `ZeroDivisionError`. O runtime do Odin já
    escreveu a explicação boa no stderr —
        /sandbox/main.odin(8:20) Index 10 is out of range 0..<3
    — e é ela que o "Quebrou aqui" deve mostrar. Devolve (mensagem, linha) ou
    None: uma morte sem essa linha (SIGSEGV cru) continua caindo no nome do sinal.

    O arquivo é escrito pelo programa do ALUNO, então nada aqui confia nele além
    de tamanho e formato: leitura limitada, uma linha só, texto recortado."""
    try:
        with io.open(STDERR_FILE, "r", encoding="utf-8", errors="replace") as fh:
            bruto = fh.read(MAX_STDERR_SCAN)
    except Exception:
        return None
    for linha in reversed([l.strip() for l in bruto.splitlines() if l.strip()]):
        m = re.match(r"^.*?\((\d+):\d+\)\s+(.+)$", linha)
        if m:
            try:
                n = int(m.group(1))
            except Exception:
                n = None
            return (_clip(m.group(2)), n)
    return None


def _codigo_de_saida():
    """O código com que o DRIVER sai, para o runner classificar o programa do
    aluno pela régua de sempre — a mesma do modo run."""
    if _fim["sinal"]:
        return 128 + SINAIS[_fim["sinal"]][0]
    try:
        return int(_fim["codigo"] or 0)
    except Exception:
        return 0


def _exec(cmd):
    try:
        gdb.execute(cmd, to_string=True)
        return True
    except gdb.error:
        return False


def collect():
    steps = []
    truncated_steps = False
    crash = None

    # Como o programa do aluno terminou só se sabe por evento — o gdb não devolve
    # isso pelo código de saída dele.
    try:
        gdb.events.exited.connect(_ao_sair)
        gdb.events.stop.connect(_ao_parar)
    except Exception:
        pass

    _exec("set confirm off")
    _exec("set pagination off")
    _exec("set height 0")
    # Send GDB's OWN output to /dev/null, keeping the inferior's untouched. The
    # student's stdout is a product surface: without this it arrives interleaved
    # with the debugger's stepping chatter —
    #   7
    #   Breakpoint 1, main::main () at main.odin:13
    #   13      n := 3
    # — and the program's real output drowns in it. `logging redirect` moves gdb's
    # stream only; the traced program keeps writing to the real stdout, which is
    # what the runner captures and shows.
    _exec("set logging file /dev/null")
    _exec("set logging redirect on")
    if not _exec("set logging enabled on"):
        _exec("set logging on")  # gdb < 12 spelling
    # A student program that segfaults must end the TRACE, not drop gdb into an
    # interactive stop we would then step forever.
    _exec("handle SIGSEGV stop nopass")

    if not _exec("break " + ENTRY):
        return steps, False, {"type": "InternalError", "message": "entrypoint %s não encontrado" % ENTRY}
    # Shell-style redirection on `run` is what separates the two streams.
    _exec("run > %s 2> %s" % (STDOUT_FILE, STDERR_FILE))

    registro = Registro()
    # Pilha de procs cuja devolução está armada, paralela aos quadros do aluno.
    devolucoes = []
    pilha_ant = None
    terminou = False
    # O último heap EMITIDO (base do delta) e os bytes já gastos no documento.
    heap_ant = None
    bytes_usados = len(json.dumps(_envelope([], False, None))) + 64

    inner = 0
    while len(steps) < MAX_STEPS and inner < MAX_GDB_STEPS:
        inner += 1
        try:
            frame = gdb.selected_frame()
            sal = frame.find_sal()
        except gdb.error:
            terminou = True
            break  # program exited

        if _in_student_file(sal):
            snap = snapshot(len(steps), registro)
            if snap is not None:
                # ── call / return ──────────────────────────────────────────────
                # O gdb entrega só "parei numa linha". Quem sabe que houve uma
                # CHAMADA é a comparação com a pilha do passo anterior: ficou mais
                # funda, entrou-se numa proc; ficou mais rasa, a proc do passo
                # anterior acabou de devolver — e é NAQUELE passo que o evento
                # `return` mora, com o quadro ainda em pé, igual ao Python.
                pilha = [f["func"] for f in snap["stack"]]
                nivel = len(pilha)
                nivel_ant = len(pilha_ant) if pilha_ant is not None else 0

                # RETORNO. O sinal não é a pilha ter encolhido — é o breakpoint de
                # saída ter DISPARADO. A diferença aparece em `f(n-1) + f(n-2)`,
                # onde a primeira chamada sai e a segunda entra sem nenhum passo no
                # nível do chamador: a profundidade fica igual e o retorno seria
                # invisível. O passo ANTERIOR é o último dentro da proc que saiu, e
                # é nele que o evento mora — com o quadro ainda em pé, como no
                # Python. Numa cascata só o mais interno tem passo onde aparecer;
                # os de fora saem sem valor, que é honesto.
                saidos = [d for d in devolucoes if d[1] is not None and d[1].saiu]
                if saidos and steps:
                    steps[-1]["event"] = "return"
                    # O VALOR só entra quando dá para PROVAR de quem ele é.
                    #
                    # A pergunta é sempre a mesma: qual passo gravado é o último
                    # de dentro do quadro que acabou de sair? Três tentativas
                    # anteriores responderam por PROFUNDIDADE ou por ORDEM DE
                    # DISPARO, e as três produziram valores plausíveis e errados —
                    # medido em `fibonacci(6)`: um quadro com `n = 0` dizendo
                    # `devolveu 8`, que é a resposta de `fib(6)`. `f(n-1) + f(n-2)`
                    # põe duas chamadas na mesma LINHA: o gdb entra e sai da
                    # primeira sem deixar passo no nível do chamador, a
                    # profundidade nunca muda, e a ordem não diz de quem é.
                    #
                    # A quarta responde por ENDEREÇO DE QUADRO (ver _id_de_quadro):
                    # o par (pc do chamador, sp do chamador). Duas chamadas na
                    # mesma linha são dois sítios de chamada, logo dois endereços
                    # de retorno; e `fib(0)` e `fib(6)` diferem no sp. Irmãs reusam
                    # o slot mas nunca coexistem, então o passo mais recente com
                    # aquela chave está sempre dentro do quadro que saiu.
                    #
                    # A regra antiga fica como PISO, e é de propósito: esta chave
                    # ainda não foi medida on-target, e uma chave que não bate tem
                    # de calar, nunca chutar. Ou seja, o caminho novo só ACRESCENTA
                    # valor onde ele é provável; ele nunca sobrepõe um silêncio com
                    # um palpite. Um valor errado ensina que `fib(0)` devolve 8;
                    # nenhum valor só não ensina.
                    anterior = pilha_ant or []
                    repetida = anterior and anterior.count(anterior[-1]) > 1
                    dono = None
                    chave_passo = steps[-1].get("_quadro")
                    if chave_passo is not None:
                        casam = [d for d in saidos if d[1].quadro == chave_passo]
                        if len(casam) == 1:
                            dono = casam[0][1]
                    if dono is None and len(saidos) == 1 and not repetida:
                        dono = saidos[0][1]
                    if dono is not None:
                        novas = _anota_devolvido(steps[-1], dono, registro, heap_ant)
                        # O passo já foi codificado (o heap dele pode ser um
                        # delta), então as caixas novas entram pela codificação
                        # dele e viram base do delta seguinte.
                        if novas:
                            _acrescenta_caixas(steps[-1], novas, heap_ant)
                if saidos:
                    devolucoes = [d for d in devolucoes if not (d[1] is not None and d[1].saiu)]

                if pilha_ant is None or nivel > nivel_ant:
                    snap["event"] = "call"
                    devolucoes.append((nivel, _arma_devolucao()))
                # Uma proc cujo breakpoint nunca dispara (gdb sem suporte, quadro
                # mais externo) não pode ficar presa segurando o nível de outra.
                devolucoes = [d for d in devolucoes if d[0] <= nivel]
                pilha_ant = pilha

                # O heap vai em DELTA, como no harness Python: o primeiro passo
                # com grafo leva o heap cheio, os seguintes levam {set, del}, e um
                # passo sem nenhum dos dois tem o heap idêntico ao anterior.
                cheio = snap.get("heap") or {}
                if heap_ant is None:
                    snap["heap"] = cheio
                else:
                    mudadas = {rid: box for rid, box in cheio.items() if heap_ant.get(rid) != box}
                    sumidas = [rid for rid in heap_ant if rid not in cheio]
                    snap.pop("heap", None)
                    if mudadas or sumidas:
                        delta = {}
                        if mudadas:
                            delta["set"] = mudadas
                        if sumidas:
                            delta["del"] = sumidas
                        snap["heap_delta"] = delta

                # ORÇAMENTO DE BYTES, medido por PASSO. Antes o teto serializava a
                # lista INTEIRA acumulada, uma vez por passo gravado, o que faz o
                # custo do próprio medidor crescer com o quadrado do número de
                # passos. Medido, só o medidor, em passos de ~818 B: 2,0 s em 533
                # passos (o `fib(10)`, ~15% dos 13 s medidos) e 46,7 s em 2500 —
                # mais que o triplo do envelope de 15 s. Ou seja, o teto de 2500
                # passos era INALCANÇÁVEL: um trace longo morria de timeout dentro
                # do próprio medidor, e o aluno recebia um erro onde deveria
                # receber um trace parcial. O harness Python sempre mediu por
                # passo; aqui era a lista toda.
                #
                # Depois, o driver INTEIRO sobre o mesmo roteiro (com o delta
                # acima): 0,04 s em 533 passos e 0,17 s em 2500, com o documento
                # caindo de 2015 KB para 577 KB. O que sobra no relógio do Odin é
                # o gdb, que é onde o custo deve mesmo estar.
                custo = len(json.dumps(snap)) + 1
                if bytes_usados + custo > MAX_REPORT_BYTES:
                    truncated_steps = True
                    break
                bytes_usados += custo
                steps.append(snap)
                # Só depois de ENTRAR é que o passo vira a base do delta seguinte —
                # senão um passo descartado pelo teto deixaria o delta seguinte
                # apontando para um heap que nunca foi enviado.
                heap_ant = cheio
            if not _exec("step"):
                terminou = True  # `step` só falha aqui quando o programa acabou
                break
        else:
            # Outside the student's file (stdlib, libc): leave without spending a
            # tutor step. `finish` fails at the outermost frame — then we are done.
            if not _exec("finish"):
                terminou = True
                break

    if len(steps) >= MAX_STEPS:
        truncated_steps = True

    # QUEBRA. Um programa que morre de sinal precisa apontar ONDE, senão o passo
    # a passo simplesmente para e a pergunta "onde quebrou?" fica sem resposta.
    # O Python já trazia esse registro; o Odin, não.
    if _fim["sinal"] and steps:
        ultimo = steps[-1]
        panico = _panico_do_odin()
        crash = {
            # A frase do próprio Odin quando ela existe; o sinal só quando não há
            # nada melhor. "Index 10 is out of range 0..<3" ensina; "SIGILL" não.
            "type": "Erro em execução" if panico else _fim["sinal"],
            "message": panico[0] if panico else SINAIS[_fim["sinal"]][1],
            "file": ultimo.get("file"),
            "line": (panico[1] if panico and panico[1] else ultimo.get("line")),
            "func": ultimo.get("func"),
        }
        ultimo["event"] = "exception"
        ultimo["exc"] = {"type": crash["type"], "message": crash["message"]}
    # O último passo de um programa que chegou INTEIRO ao fim é o retorno da proc
    # mais externa — não há passo SEGUINTE onde a pilha encolheria, então ele
    # nunca seria marcado pela comparação. É o mesmo `return` do `<module>` no
    # Python. Num trace cortado, o último passo é onde a régua acabou, não onde o
    # programa acabou — e num que quebrou, o último passo é a quebra.
    elif terminou and steps and not truncated_steps:
        steps[-1]["event"] = "return"
        _acrescenta_caixas(
            steps[-1],
            _anota_devolvido(steps[-1], devolucoes[-1][1] if devolucoes else None, registro, heap_ant),
            heap_ant,
        )

    # Let the program RUN TO COMPLETION even when the step budget ran out: it is
    # still the student's program, its remaining output is theirs, and a process
    # killed mid-way never flushes its buffers (measured: an unfinished inferior
    # leaves the stdout file empty). Errors here just mean it already exited.
    _exec("continue")
    # O último `stdout_len` só é honesto depois do `continue`: é lá que o resto da
    # saída do programa sai. Sem isto o último passo de um trace cortado mostraria
    # menos saída do que o painel "Resultado" ao lado.
    if steps:
        steps[-1]["stdout_len"] = max(steps[-1].get("stdout_len", 0), _stdout_ate_agora())

    # A truncagem de heap é do DOCUMENTO, não de um passo: basta um passo ter
    # batido no teto para o desenho estar incompleto em algum lugar.
    heap_truncado = any(p.get("heap_truncated") for p in steps)
    return steps, truncated_steps, crash, heap_truncado


def main():
    try:
        steps, truncated, crash, heap_truncado = collect()
    except Exception as e:  # a driver bug must not look like a student failure
        steps, truncated, crash, heap_truncado = [], False, {"type": "InternalError", "message": _clip(e)}, False

    # Campo de trabalho, nunca do contrato: a chave do quadro serve para atribuir
    # o valor devolvido e não tem sentido para quem consome o trace.
    for p in steps:
        p.pop("_quadro", None)

    doc = _envelope(steps, truncated, crash, heap_truncado)
    blob = json.dumps(doc)
    if len(blob) > MAX_REPORT_BYTES:
        # Rede de segurança: o orçamento por passo já para antes de chegar aqui,
        # mas se o envelope estourar por outro caminho é melhor entregar metade do
        # trace do que um JSON que o cliente não consegue ler. Cortar no fim
        # preserva o começo, que é onde o aluno está olhando.
        doc = _envelope(steps[: max(0, len(steps) // 2)], truncated, crash, heap_truncado, True)
        blob = json.dumps(doc)
    with open(REPORT, "w") as fh:
        fh.write(blob)


main()
# Sai com o código do PROGRAMA DO ALUNO, não com o do gdb. É isso que faz o
# runner classificar um trace igual a um run: sem isto, `xs[10]` numa fatia de 3
# dava runtime_error em modo run e "sucesso" em modo trace.
gdb.execute("quit %d" % _codigo_de_saida(), to_string=True)
