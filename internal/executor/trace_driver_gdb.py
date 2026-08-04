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
#   * a BARE pointer is never dereferenced — only its address is reported. The
#     ONE exception is a container whose layout the language defines (an Odin
#     string/slice/dynamic array is {data,len[,cap]}): there we DO read through
#     `data`, but only after sanity-checking `len`, only up to MAX_ITEMS, and with
#     every element read guarded — a scribbled length or a dangling buffer yields
#     `opaque`, never a crash. Without this the student sees `{data: 0x5555…,
#     len: 3}` instead of `"ola"` or `[10, 20, 30]`, which is the implementation,
#     not the value;
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
# a per-trace counter so no real address ever reaches the client.
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
        # endereço real → id da caixa que mora ali. É o que permite ligar um
        # ponteiro à caixa certa SEM desreferenciá-lo (ver _liga_ponteiros).
        self.por_endereco = {}

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


def _chave(marca, valor, ponteiro, tipo):
    """A chave que identifica uma caixa. Normalmente é o PONTEIRO DOS DADOS: duas
    fatias sobre o mesmo arranjo são a mesma coisa, e passar uma para uma proc
    mantém a caixa — que é o comportamento certo.

    Menos quando o ponteiro é NULO. Aí ele não identifica nada, e dois `[dynamic]`
    vazios distintos caíam na MESMA caixa: o diagrama desenhava duas setas para um
    objeto só, ensinando que `a` e `b` são aliases quando não são — o oposto
    exato do que ele existe para mostrar. Nesse caso a identidade passa a ser o
    endereço do próprio valor, que distingue as duas variáveis."""
    if _ehNulo(ponteiro):
        try:
            propria = valor.address
            if propria is not None:
                return (marca, "vazio@" + str(propria), str(tipo))
        except Exception:
            pass
    return (marca, str(ponteiro), str(tipo))


def _odin_sequence(v, t, heap, depth, kind_name):
    """Odin slice `[]T` / `[dynamic]T` -> a list box with real cells. Both are
    {data,len[,cap]}; `cap` is shown for a dynamic array because "grew to 8 slots
    holding 1" is exactly the thing a student needs to see."""
    n = _sane_len(v)
    if n is None:
        return _opaque("comprimento inválido")
    key = _chave("seq", v, v["data"], t)
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
            return _prim("ptr", "nil" if n == 0 else "0x%x" % n)

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
    yet (see the header: DWARF scope != defined)."""
    out = {}
    try:
        block = frame.block()
    except Exception:
        return out
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
    return out


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
    for fr in reversed(frames):
        try:
            sal = fr.find_sal()
            if not _in_student_file(sal):
                continue
            stack.append({
                "func": _nome_curto(fr.name()),
                "file": os.path.basename(sal.symtab.filename),
                "line": sal.line,
                "vars": frame_vars(fr, heap, sal.line),
            })
        except Exception:
            continue

    try:
        cur = gdb.selected_frame().find_sal()
        line = cur.line
        func = _nome_curto(gdb.selected_frame().name())
    except Exception:
        return None

    # Agora que TODAS as caixas do passo existem, um ponteiro pode ser ligado à
    # caixa que ele endereça. Tem de ser aqui, no fim: quando `a.prox` é lido, a
    # caixa de `b` pode ainda não ter sido montada.
    _liga_ponteiros(stack, heap.boxes, heap.por_endereco)

    # O registro guarda só o que estava vivo NESTE passo: é essa poda que impede
    # um endereço reaproveitado de herdar a caixa de um objeto morto.
    registro.podar(heap.usados)

    return {
        "step": step_no,
        # O evento definitivo é decidido em `collect`, comparando a pilha com a
        # do passo anterior: só de lá dá para ver que uma proc foi chamada ou que
        # ela acabou de devolver.
        "event": "line",
        "file": TARGET,
        "line": line,
        "func": func,
        "stdout_len": 0,
        "stack": stack,
        "heap": heap.boxes,
    }


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

    def stop(self):
        try:
            self.valor = self.return_value
        except Exception:
            self.valor = None
        return False

    def out_of_scope(self):
        # O quadro morreu sem passar pelo retorno (sinal, longjmp): sem valor.
        self.valor = None


def _arma_devolucao():
    """Arma a captura para o quadro ATUAL, se der. Falha silenciosa: no quadro
    mais externo não há endereço de retorno, e um gdb sem suporte deve custar o
    valor devolvido, não o trace inteiro."""
    try:
        return _Devolucao(gdb.selected_frame())
    except Exception:
        return None


def _anota_devolvido(passo, bp, registro):
    """Prega o valor devolvido no passo de `return`, no heap DAQUELE passo."""
    if bp is None or bp.valor is None:
        return
    try:
        heap = Heap(registro, passo.get("heap"))
        passo["retval"] = value_v2(bp.valor, heap)
        passo["heap"] = heap.boxes
    except Exception:
        pass


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
                if pilha_ant is None or len(pilha) > len(pilha_ant):
                    snap["event"] = "call"
                    devolucoes.append(_arma_devolucao())
                elif len(pilha) < len(pilha_ant):
                    quantas = len(pilha_ant) - len(pilha)
                    if steps:
                        steps[-1]["event"] = "return"
                        # A proc mais interna é a última armada; as intermediárias
                        # (retorno em cascata num passo só) saem sem valor, que é
                        # honesto: não houve passo onde mostrá-las.
                        bp = None
                        for _ in range(quantas):
                            if devolucoes:
                                bp = devolucoes.pop()
                        _anota_devolvido(steps[-1], bp, registro)
                    else:
                        for _ in range(quantas):
                            if devolucoes:
                                devolucoes.pop()
                pilha_ant = pilha

                steps.append(snap)
                # Size cap: stop while the document is still valid JSON.
                if len(json.dumps(steps)) > MAX_REPORT_BYTES:
                    steps.pop()
                    truncated_steps = True
                    break
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
        _anota_devolvido(steps[-1], devolucoes.pop() if devolucoes else None, registro)

    # Let the program RUN TO COMPLETION even when the step budget ran out: it is
    # still the student's program, its remaining output is theirs, and a process
    # killed mid-way never flushes its buffers (measured: an unfinished inferior
    # leaves the stdout file empty). Errors here just mean it already exited.
    _exec("continue")
    return steps, truncated_steps, crash


def main():
    try:
        steps, truncated, crash = collect()
    except Exception as e:  # a driver bug must not look like a student failure
        steps, truncated, crash = [], False, {"type": "InternalError", "message": _clip(e)}

    doc = {
        "version": TRACE_VERSION,
        "language": LANGUAGE,
        "steps": steps,
        "step_count": len(steps),
        "truncated": {"steps": truncated, "bytes": False},
    }
    if crash:
        doc["crash"] = crash

    blob = json.dumps(doc)
    if len(blob) > MAX_REPORT_BYTES:
        doc["steps"] = doc["steps"][: max(0, len(doc["steps"]) // 2)]
        doc["truncated"] = {"steps": truncated, "bytes": True}
        blob = json.dumps(doc)
    with open(REPORT, "w") as fh:
        fh.write(blob)


main()
# Sai com o código do PROGRAMA DO ALUNO, não com o do gdb. É isso que faz o
# runner classificar um trace igual a um run: sem isto, `xs[10]` numa fatia de 3
# dava runtime_error em modo run e "sucesso" em modo trace.
gdb.execute("quit %d" % _codigo_de_saida(), to_string=True)
