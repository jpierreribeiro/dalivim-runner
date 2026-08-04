# Testes de COMPORTAMENTO do driver de gdb do Odin, contra o `gdb` falso.
#
# Complementa odin_test.go, que afirma sobre o TEXTO do driver: aqui o driver é
# executado. Cada teste abaixo corresponde a um defeito real (ou a uma garantia
# que não tinha nenhuma rede).
#
# Rodado pelo Go: TestOdinTraceDriver_Behaviour em odin_test.go.

import json
import os
import sys
import tempfile

AQUI = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, AQUI)

import trace_driver_gdb_fake as fake  # noqa: E402

falhas = []


def checa(nome, cond, detalhe=""):
    if cond:
        print("ok    %s" % nome)
    else:
        print("FALHA %s  %s" % (nome, detalhe))
        falhas.append(nome)


def roda(programa, saidas=None, env=None):
    """Executa o driver de verdade sobre um programa roteirizado e devolve o
    documento que ele escreveu."""
    tmp = tempfile.mkdtemp()
    relatorio = os.path.join(tmp, "trace.json")
    saida = os.path.join(tmp, "stdout.txt")
    open(saida, "w").close()

    os.environ["DALIVIM_TRACE_TARGET"] = programa.arquivo
    os.environ["DALIVIM_TRACE_REPORT"] = relatorio
    os.environ["DALIVIM_TRACE_STDOUT"] = saida
    os.environ["DALIVIM_TRACE_STDERR"] = os.path.join(tmp, "stderr.txt")
    for k, v in (env or {}).items():
        os.environ[k] = str(v)
    os.environ.setdefault("DALIVIM_TRACE_MAX_STEPS", "2500")
    os.environ.setdefault("DALIVIM_TRACE_MAX_REPORT_BYTES", "2000000")

    fake.instalar(programa, saida_arquivo=saida)
    for m in list(sys.modules):
        if m == "trace_driver_gdb":
            del sys.modules[m]
    fonte = open(os.path.join(AQUI, "trace_driver_gdb.py")).read()
    escopo = {"__name__": "trace_driver_gdb"}
    try:
        exec(compile(fonte, "trace_driver_gdb.py", "exec"), escopo)
    except SystemExit:
        pass
    for k in ("DALIVIM_TRACE_MAX_STEPS", "DALIVIM_TRACE_MAX_REPORT_BYTES"):
        os.environ.pop(k, None)
    return json.load(open(relatorio))


def heaps_remontados(doc):
    """A mesma remontagem que o frontend faz. Verificar o fio em vez do que o
    aluno vê seria verificar outra coisa."""
    out = []
    corrente = {}
    for s in doc["steps"]:
        if "heap" in s:
            corrente = dict(s["heap"])
        elif "heap_delta" in s:
            corrente = dict(corrente)
            corrente.update(s["heap_delta"].get("set") or {})
            for k in s["heap_delta"].get("del") or []:
                corrente.pop(k, None)
        out.append(corrente)
    return out


def fatia(celulas, tipo="[]int", data_off=0, n=None):
    """Uma fatia do Odin: a struct {data,len} que o DWARF descreve."""
    vals = [fake.Value(c, fake.INT) for c in celulas]
    t = fake.Type(tipo, fake.TYPE_CODE_STRUCT,
                  campos=[("data", fake.INT), ("len", fake.INT)])
    bruto = {"data": fake.Value((vals, data_off), fake.INT),
             "len": fake.Value(len(celulas) - data_off if n is None else n, fake.INT)}
    return fake.Value(bruto, t, endereco=None)


def inteiro(n):
    return fake.Value(n, fake.INT)


# ── F3: uma sub-fatia não pode herdar a caixa da fatia inteira ───────────────
#
#   a := []int{1, 2, 3}
#   b := a[:2]
#
# `b.data == a.data` e o tipo é o mesmo, então a chave era idêntica — e como a
# caixa só é montada na primeira vez, as duas setas apontavam para uma caixa de
# TRÊS itens. O aluno lia que `b` é a mesma fatia de `a`, contra o `len(b) == 2`
# do próprio programa dele.
def teste_subfatia_nao_herda_a_caixa():
    celulas = [inteiro(1), inteiro(2), inteiro(3)]
    tA = fake.Type("[]int", fake.TYPE_CODE_STRUCT, campos=[("data", fake.INT), ("len", fake.INT)])
    a = fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(3)}, tA)
    b = fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(2)}, tA)
    quadro = lambda linha: {
        "func": "main::main", "line": linha, "sp": 100, "decl": 1,
        "syms": [fake.Symbol("a", a, 2), fake.Symbol("b", b, 3)],
    }
    doc = roda(fake.Programa([[quadro(2)], [quadro(3)], [quadro(4)], [quadro(5)]]))
    heaps = heaps_remontados(doc)
    ultimo = doc["steps"][-1]
    vars_ = ultimo["stack"][-1]["vars"]
    ra, rb = vars_.get("a", {}).get("ref"), vars_.get("b", {}).get("ref")
    checa("F3 a e b têm caixas distintas", ra and rb and ra != rb, "a=%s b=%s" % (ra, rb))
    if ra and rb and ra != rb:
        checa("F3 a tem 3 itens", len(heaps[-1][ra]["items"]) == 3, str(heaps[-1][ra]))
        checa("F3 b tem 2 itens", len(heaps[-1][rb]["items"]) == 2, str(heaps[-1][rb]))


# Mas duas fatias IGUAIS continuam sendo a mesma caixa — é o que `copia := notas`
# tem de ensinar, e o corpus afirma isso (OD_ALIAS).
def teste_fatias_iguais_continuam_alias():
    celulas = [inteiro(7), inteiro(8), inteiro(9)]
    t = fake.Type("[]int", fake.TYPE_CODE_STRUCT, campos=[("data", fake.INT), ("len", fake.INT)])
    mk = lambda: fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(3)}, t)
    quadro = lambda linha: {
        "func": "main::main", "line": linha, "sp": 100, "decl": 1,
        "syms": [fake.Symbol("notas", mk(), 2), fake.Symbol("copia", mk(), 3)],
    }
    doc = roda(fake.Programa([[quadro(3)], [quadro(4)], [quadro(5)]]))
    vars_ = doc["steps"][-1]["stack"][-1]["vars"]
    checa("alias preservado", vars_["notas"]["ref"] == vars_["copia"]["ref"],
          "%s vs %s" % (vars_["notas"], vars_["copia"]))


# ── heap em DELTA (item 3 da seção 7) ────────────────────────────────────────
def teste_heap_viaja_em_delta():
    celulas = [inteiro(1)]
    t = fake.Type("[]int", fake.TYPE_CODE_STRUCT, campos=[("data", fake.INT), ("len", fake.INT)])
    v = fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(1)}, t)
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1,
                            "syms": [fake.Symbol("xs", v, 2)]}
    doc = roda(fake.Programa([[quadro(i)] for i in range(3, 12)]))
    checa("delta anunciado", doc.get("heap_encoding") == "delta", str(doc.get("heap_encoding")))
    cheios = [s for s in doc["steps"] if "heap" in s]
    checa("um único passo com heap cheio", len(cheios) == 1, "%d passos com heap cheio" % len(cheios))
    # E o heap remontado tem de continuar dizendo a mesma coisa em todo passo.
    heaps = heaps_remontados(doc)
    rid = doc["steps"][-1]["stack"][-1]["vars"]["xs"]["ref"]
    checa("remontagem preserva a caixa", all(rid in h for h in heaps[1:]),
          "caixa %s sumiu na remontagem" % rid)


# ── F5: stdout_len deixa de ser 0 ────────────────────────────────────────────
def teste_stdout_len_acompanha_a_saida():
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1, "syms": []}
    prog = fake.Programa([[quadro(2)], [quadro(3)], [quadro(4)], [quadro(5)]],
                         saidas={1: "ola\n", 2: "mundo\n"})
    doc = roda(prog)
    lens = [s["stdout_len"] for s in doc["steps"]]
    # A sequência EXATA, e não só "cresce": com o `0` fixo de antes o último passo
    # ainda ganhava o total no fim de collect, então uma verificação frouxa
    # ("termina > 0") passava com o defeito inteiro no lugar. O que o painel
    # "Saída até aqui" precisa é justamente dos passos DO MEIO.
    checa("stdout_len é o prefixo certo em cada passo", lens == [0, 4, 10, 10], str(lens))


def teste_stdout_len_conta_code_points():
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1, "syms": []}
    prog = fake.Programa([[quadro(2)], [quadro(3)], [quadro(4)]], saidas={1: "ação\n"})
    doc = roda(prog)
    # "ação\n" tem 5 code points e 6 bytes em UTF-8. O frontend fatia por code
    # point (Array.from), então contar bytes mostraria saída a mais.
    checa("acentuado conta 5, não 6", doc["steps"][-1]["stdout_len"] == 5,
          str([s["stdout_len"] for s in doc["steps"]]))


# ── F6: o teto de caixas por passo deixa de ser invisível ────────────────────
def teste_teto_de_caixas_aparece_no_documento():
    t = fake.Type("[]int", fake.TYPE_CODE_STRUCT, campos=[("data", fake.INT), ("len", fake.INT)])
    syms = []
    for i in range(30):
        celulas = [inteiro(i)]
        syms.append(fake.Symbol("v%d" % i, fake.Value(
            {"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(1)}, t), 1))
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 0, "syms": syms}
    doc = roda(fake.Programa([[quadro(2)], [quadro(3)]]),
               env={"DALIVIM_TRACE_MAX_STEPS": "2500"})
    # Com o teto padrão (200) 30 caixas cabem; o que se verifica aqui é que a
    # flag EXISTE no envelope, em vez de ser escrita e descartada como antes.
    checa("truncated.heap existe no envelope", "heap" in doc["truncated"], str(doc["truncated"]))
    checa("truncated.heap é falso quando cabe", doc["truncated"]["heap"] is False, str(doc["truncated"]))


# ── 5.1 quarta abordagem: o valor devolvido em recursão ─────────────────────
#
# `fib(n-1) + fib(n-2)`: duas chamadas na MESMA linha. O gdb entra e sai da
# primeira sem deixar passo no nível do chamador, então nem a profundidade nem a
# ordem de disparo identificam o quadro — foi assim que se produziu um quadro com
# `n = 0` dizendo `devolveu 8`, que é a resposta de fib(6).
#
# O roteiro abaixo é justamente esse: main chama fib(2), que chama fib(1) e
# depois fib(0), do mesmo sítio de chamada não — de dois sítios distintos na
# mesma linha, que é o que o compilador gera.
def teste_retorno_em_recursao_nao_mente():
    def fib(n, sp, call_pc, linha, retorna=None):
        return {"func": "main::fib", "line": linha, "sp": sp, "call_pc": call_pc, "decl": 3,
                "syms": [fake.Symbol("n", inteiro(n), 3, argumento=True)], "retorna": retorna}
    principal = {"func": "main::main", "line": 20, "sp": 900, "call_pc": 0, "decl": 19, "syms": []}

    # call_pc 41 = sítio de `fib(n-1)`; 42 = sítio de `fib(n-2)`. Mesma linha 5.
    paradas = [
        [principal],                                        # 0
        [principal, fib(2, 800, 30, 4)],                     # 1  entra em fib(2)
        [principal, fib(2, 800, 30, 5)],                     # 2  linha do `f(n-1)+f(n-2)`
        [principal, fib(2, 800, 30, 5), fib(1, 700, 41, 4)],  # 3  entra em fib(1)
        [principal, fib(2, 800, 30, 5), fib(1, 700, 41, 5, retorna=inteiro(1))],  # 4 último de fib(1)
        [principal, fib(2, 800, 30, 5), fib(0, 700, 42, 4)],  # 5  entra em fib(0), MESMO sp
        [principal, fib(2, 800, 30, 5), fib(0, 700, 42, 5, retorna=inteiro(0))],  # 6 último de fib(0)
        [principal, fib(2, 800, 30, 6, retorna=inteiro(1))],  # 7  volta a fib(2)
        [principal],                                         # 8
    ]
    doc = roda(fake.Programa(paradas))

    FIB = [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]
    mentiras = []
    mostrados = 0
    for s in doc["steps"]:
        if "retval" not in s:
            continue
        topo = s["stack"][-1] if s["stack"] else {}
        v = (topo.get("vars") or {}).get("n")
        if not v or "text" not in v or not v["text"].lstrip("-").isdigit():
            continue
        n = int(v["text"])
        texto = s["retval"].get("text", "")
        if not texto.lstrip("-").isdigit():
            continue
        mostrados += 1
        if 0 <= n < len(FIB) and FIB[n] != int(texto):
            mentiras.append("n=%d diz devolveu %s (fib=%d)" % (n, texto, FIB[n]))
    checa("retorno nunca mente em recursão", not mentiras, "; ".join(mentiras))
    # E o ponto da quarta abordagem: em recursão o valor agora APARECE, em vez de
    # ser omitido por não dar para provar de quem era.
    checa("retorno aparece em recursão", mostrados >= 2,
          "só %d quadros recursivos mostraram devolveu" % mostrados)


# O caso simples continua funcionando: `dobro(21)` → `devolveu 42`.
def teste_retorno_simples():
    principal = {"func": "main::main", "line": 9, "sp": 900, "call_pc": 0, "decl": 8, "syms": []}
    dobro = lambda linha, ret=None: {
        "func": "main::dobro", "line": linha, "sp": 700, "call_pc": 55, "decl": 3,
        "syms": [fake.Symbol("n", inteiro(21), 3, argumento=True)], "retorna": ret}
    doc = roda(fake.Programa([
        [principal],
        [principal, dobro(4)],
        [principal, dobro(5, ret=inteiro(42))],
        [principal],
    ]))
    devolvidos = [s["retval"]["text"] for s in doc["steps"] if "retval" in s]
    checa("dobro(21) devolveu 42", "42" in devolvidos, str(devolvidos))
    eventos = {}
    for s in doc["steps"]:
        eventos[s["event"]] = eventos.get(s["event"], 0) + 1
    checa("houve call e return", eventos.get("call", 0) >= 1 and eventos.get("return", 0) >= 1,
          str(eventos))


# ── 5.4: o passo de `call` não pode dizer "sem variáveis" ────────────────────
def teste_prologo_e_marcado():
    principal = {"func": "main::main", "line": 9, "sp": 900, "call_pc": 0, "decl": 8, "syms": []}
    # Parado NA linha de declaração da proc: o argumento existe mas ainda não é
    # legível (o prólogo não salvou os registradores).
    entrada = {"func": "main::dobro", "line": 3, "sp": 700, "call_pc": 55, "decl": 3,
               "syms": [fake.Symbol("n", inteiro(21), 3, argumento=True)]}
    depois = dict(entrada, line=4)
    doc = roda(fake.Programa([[principal], [principal, entrada], [principal, depois], [principal]]))
    # O passo em que se ENTRA em dobro: pilha mais funda, parado na linha da
    # declaração da proc.
    entrou = next((s for s in doc["steps"]
                   if s["event"] == "call" and s["stack"][-1]["func"] == "dobro"), None)
    checa("existe passo de call em dobro", entrou is not None,
          str([(s["event"], s["stack"][-1]["func"]) for s in doc["steps"]]))
    if entrou:
        topo = entrou["stack"][-1]
        checa("argumento escondido no prólogo", not topo["vars"], str(topo["vars"]))
        # Sem esta marca o desenho rotula o quadro "sem variáveis", que AFIRMA que
        # `dobro` não tem parâmetros — mais forte que "ainda não dá para lê-los".
        checa("e o passo diz que é o prólogo", topo.get("prologo") is True, str(topo))
    achou = any(f.get("vars", {}).get("n") for s in doc["steps"] for f in s["stack"])
    checa("argumento aparece depois do prólogo", achou)


# ── variável escondida até a linha que a declara (garantia existente, sem rede) ──
def teste_variavel_escondida_ate_ser_declarada():
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1,
                            "syms": [fake.Symbol("n", inteiro(3), 16)]}
    doc = roda(fake.Programa([[quadro(15)], [quadro(16)], [quadro(17)], [quadro(18)]]))
    por_linha = {s["line"]: s["stack"][-1]["vars"] for s in doc["steps"]}
    checa("escondida antes da linha 16", not por_linha.get(15), str(por_linha.get(15)))
    checa("escondida NA linha 16", not por_linha.get(16), str(por_linha.get(16)))
    checa("visível na linha 17", "n" in (por_linha.get(17) or {}), str(por_linha.get(17)))


# ── ponteiro sem forma declarada nunca é desreferenciado ─────────────────────
def teste_ponteiro_pendurado_degrada():
    tNode = fake.Type("Node", fake.TYPE_CODE_STRUCT, campos=[("valor", fake.INT)])
    tPtr = fake.Type("Node *", fake.TYPE_CODE_PTR, alvo=tNode)
    pendurado = fake.Value(0xDEAD, tPtr, memoria={})  # não há alvo: dereference levanta
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1,
                            "syms": [fake.Symbol("p", pendurado, 2)]}
    doc = roda(fake.Programa([[quadro(3)], [quadro(4)]]))
    v = doc["steps"][-1]["stack"][-1]["vars"]["p"]
    checa("ponteiro pendurado vira endereço, não derruba", v.get("prim") == "ptr", str(v))
    checa("driver sobreviveu", len(doc["steps"]) == 2, str(len(doc["steps"])))


# ── o orçamento de bytes corta sem quebrar o JSON (F1) ──────────────────────
def teste_teto_de_bytes_corta_e_marca():
    quadro = lambda linha: {"func": "main::main", "line": linha, "sp": 100, "decl": 1,
                            "syms": [fake.Symbol("n", inteiro(linha), 1)]}
    doc = roda(fake.Programa([[quadro(i)] for i in range(2, 200)]),
               env={"DALIVIM_TRACE_MAX_REPORT_BYTES": "3000"})
    checa("cortou antes do teto", 0 < len(doc["steps"]) < 198, str(len(doc["steps"])))
    checa("marcou a truncagem", doc["truncated"]["steps"] is True, str(doc["truncated"]))
    checa("documento continua JSON válido", isinstance(doc["steps"], list))


# ── a fixture do contrato, para o outro lado do fio ─────────────────────────
#
# Os dois lados eram testados separadamente: o runner verificava o que escreve, o
# frontend verificava documentos escritos à mão. Nenhum dos dois pegaria uma
# divergência de contrato — foi assim que o formato @2 chegou a produção sem
# estar documentado, e é o mesmo tipo de lacuna que deixou o `stdout_len` fixo em
# 0 sem nada acusar.
#
#   python3 trace_driver_gdb_test.py --fixture ../../../dalivim-frontend/src/lib/runner/__fixtures__/odin-trace.json
def programa_do_contrato():
    """Um programa que exercita o formato inteiro: fatia, SUB-fatia (mesmo
    ponteiro de dados, comprimento diferente), chamada, prólogo, retorno com
    valor, delta do heap e saída acumulada."""
    celulas = [inteiro(10), inteiro(20), inteiro(30)]
    t = fake.Type("[]int", fake.TYPE_CODE_STRUCT, campos=[("data", fake.INT), ("len", fake.INT)])
    todos = fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(3)}, t)
    parte = fake.Value({"data": fake.Value((celulas, 0), fake.INT), "len": inteiro(2)}, t)
    principal = lambda l: {
        "func": "main::main", "line": l, "sp": 900, "call_pc": 0, "decl": 1,
        "syms": [fake.Symbol("todos", todos, 2), fake.Symbol("parte", parte, 3)]}
    dobro = lambda l, ret=None: {
        "func": "main::dobro", "line": l, "sp": 700, "call_pc": 55, "decl": 6,
        "syms": [fake.Symbol("n", inteiro(21), 6, argumento=True)], "retorna": ret}
    return fake.Programa(
        [[principal(2)], [principal(3)], [principal(4)],
         [principal(4), dobro(6)], [principal(4), dobro(7, ret=inteiro(42))],
         [principal(5)]],
        saidas={4: "42\n"})


def escreve_fixture(caminho):
    doc = roda(programa_do_contrato())
    with open(caminho, "w") as fh:
        json.dump(doc, fh, indent=1, ensure_ascii=False)
        fh.write("\n")
    print("fixture escrita em %s (%d passos)" % (caminho, len(doc["steps"])))


# E a mesma fixture é verificada AQUI, para o gerador não poder produzir um
# documento que o outro lado rejeitaria sem ninguém notar entre uma geração e
# outra.
def teste_zz_fixture_do_contrato():
    doc = roda(programa_do_contrato())
    checa("contrato: delta anunciado", doc.get("heap_encoding") == "delta")
    checa("contrato: um heap cheio só", len([s for s in doc["steps"] if "heap" in s]) == 1)
    heaps = heaps_remontados(doc)
    passo = next((i for i, s in enumerate(doc["steps"])
                  if (s["stack"][-1].get("vars") or {}).get("parte")), None)
    checa("contrato: passo com as duas fatias", passo is not None)
    if passo is not None:
        v = doc["steps"][passo]["stack"][-1]["vars"]
        ra, rb = v["todos"]["ref"], v["parte"]["ref"]
        checa("contrato: caixas distintas", ra != rb)
        checa("contrato: 3 e 2 itens",
              len(heaps[passo][ra]["items"]) == 3 and len(heaps[passo][rb]["items"]) == 2)
    checa("contrato: devolveu 42", any(s.get("retval", {}).get("text") == "42" for s in doc["steps"]))
    checa("contrato: prólogo marcado",
          any(f.get("prologo") for s in doc["steps"] for f in s["stack"]))
    checa("contrato: saída acumulada", doc["steps"][-1]["stdout_len"] > 0)


TESTES = [v for k, v in sorted(globals().items()) if k.startswith("teste_")]

if __name__ == "__main__":
    if "--fixture" in sys.argv:
        escreve_fixture(sys.argv[sys.argv.index("--fixture") + 1])
        sys.exit(0)
    for t in TESTES:
        t()
    print("\n%d de %d verificações falharam." % (len(falhas), len(falhas) + 0) if falhas else "\ntudo verde.")
    sys.exit(1 if falhas else 0)
