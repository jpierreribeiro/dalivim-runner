#!/usr/bin/env python3
"""Corpus de exemplos do passo a passo, Python e Odin, com verificação.

Existe porque olhar caso a caso não escala e deixou passar um erro real: um
`fibonacci` em Odin mostrava um quadro com `n = 0` dizendo `devolveu 8`. Cada
exemplo aqui declara o que o desenho TEM de dizer, e o script confere.

    RUNNER=http://127.0.0.1:8099 python3 scripts/tutor-corpus.py

14 exemplos: 6 em Python e 8 em Odin. Sai 0 se todos passam. Não substitui os
testes Go — é a rede que pega o que só aparece com um programa de verdade
percorrido de ponta a ponta.

DUAS LIÇÕES QUE ESTE ARQUIVO JÁ APRENDEU, e que valem para quem for acrescentar
um exemplo:

* uma verificação que só reprova o caso RUIM passa de graça quando o produtor
  deixa de emitir o dado. Foi o que aconteceu com `retorno_nunca_mente`: quando
  os retornos ambíguos do Odin passaram a ser omitidos, o verificador que pegou o
  bug do fibonacci virou vacuamente verdadeiro, porque ele pula todo passo sem
  `retval`. Por isso agora existe `devolveu`, que cobra o caso POSITIVO;
* um exemplo que verifica só o `stdout` verifica que o programa RODA, não que o
  desenho diz o que ele existe para dizer. O exemplo "py ciclo" era assim.

E o que este script NÃO alcança, por construção: ele fala direto com o runner,
com um timeout largo. A classe de bug em que o backend corta uma resposta
legítima do runner (envelope de tempo, prazo coordenado) é invisível daqui —
para essa, o único teste é abrir a tela do aluno.
"""
import json
import os
import sys
import urllib.request

RUNNER = os.environ.get("RUNNER", "http://127.0.0.1:8099")
TOKEN = os.environ.get("RUNNER_TOKEN", "")


def trace(lang, src):
    cab = {"content-type": "application/json"}
    if TOKEN:
        cab["X-Runner-Token"] = TOKEN
    req = urllib.request.Request(
        RUNNER + "/run",
        data=json.dumps({"language": lang, "mode": "trace", "source_code": src}).encode(),
        headers=cab,
    )
    return json.loads(urllib.request.urlopen(req, timeout=90).read())


# ── as perguntas que cada exemplo faz ao desenho ─────────────────────────────

def stdout_e(esperado):
    return lambda r, d: (
        None if r["stdout"] == esperado else "stdout %r != %r" % (r["stdout"], esperado)
    )


def status_e(esperado):
    return lambda r, d: (
        None if r["status"] == esperado else "status %s != %s" % (r["status"], esperado)
    )


def remonta_heaps(d):
    """Preenche `heap` em TODO passo, remontando os deltas.

    Sem isto o verificador mente: o Python manda `heap_encoding: "delta"` — só o
    primeiro passo com grafo traz o heap cheio — e um checker que olha apenas a
    chave `heap` conclui que não há objeto nenhum. É a mesma remontagem que o
    frontend faz; fazer diferente aqui seria verificar algo que ninguém vê.
    """
    delta = d.get("heap_encoding") == "delta"
    corrente = None
    for s in d.get("steps", []):
        if "heap" in s:
            corrente = s["heap"]
        elif "heap_delta" in s:
            base = dict(corrente or {})
            base.update(s["heap_delta"].get("set") or {})
            for k in s["heap_delta"].get("del") or []:
                base.pop(k, None)
            corrente = base
        elif not delta:
            corrente = None
        s["heap"] = corrente or {}
    return d


def _caixas(passo):
    return passo.get("heap") or {}


def _refs_de(passo, nome):
    """Ids que a variável `nome` alcança, em qualquer quadro do passo."""
    saida = []
    for f in passo.get("stack", []):
        v = (f.get("vars") or {}).get(nome)
        if isinstance(v, dict) and "ref" in v:
            saida.append(v["ref"])
    return saida


def alias(a, b):
    """Duas variáveis têm de apontar para a MESMA caixa — o coração do diagrama."""
    def checa(r, d):
        for passo in reversed(d["steps"]):
            ra, rb = _refs_de(passo, a), _refs_de(passo, b)
            if ra and rb:
                return None if ra[0] == rb[0] else "%s=%s e %s=%s deviam ser a mesma caixa" % (a, ra[0], b, rb[0])
        return "nenhum passo teve %s e %s juntos" % (a, b)
    return checa


def nao_alias(a, b):
    def checa(r, d):
        for passo in reversed(d["steps"]):
            ra, rb = _refs_de(passo, a), _refs_de(passo, b)
            if ra and rb:
                return None if ra[0] != rb[0] else "%s e %s colapsaram na caixa %s" % (a, b, ra[0])
        return "nenhum passo teve %s e %s juntos" % (a, b)
    return checa


def cadeia_de(n):
    """n caixas ligadas por setas — uma lista ligada desenhada de verdade."""
    def checa(r, d):
        melhor = 0
        for passo in d["steps"]:
            setas = sum(
                1
                for b in _caixas(passo).values()
                for v in (b.get("fields") or {}).values()
                if isinstance(v, dict) and "ref" in v
            )
            melhor = max(melhor, setas)
        return None if melhor >= n - 1 else "esperava >=%d setas entre objetos, vi %d" % (n - 1, melhor)
    return checa


def id_estavel(nome):
    """A caixa que a variável alcança não pode trocar de id no meio do caminho."""
    def checa(r, d):
        vistos = []
        for passo in d["steps"]:
            for rid in _refs_de(passo, nome)[:1]:
                if not vistos or vistos[-1] != rid:
                    vistos.append(rid)
        return None if len(vistos) <= 1 else "%s trocou de caixa: %s" % (nome, " → ".join(vistos))
    return checa


def eventos(minimos):
    def checa(r, d):
        cont = {}
        for s in d["steps"]:
            cont[s["event"]] = cont.get(s["event"], 0) + 1
        faltando = [k for k, v in minimos.items() if cont.get(k, 0) < v]
        return None if not faltando else "faltou %s (tem %s)" % (faltando, cont)
    return checa


FIB = [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]


def retorno_nunca_mente(param="n"):
    """Todo `devolveu` exibido tem de bater com o parâmetro do quadro.

    Foi esta pergunta que pegou o erro do fibonacci. Omitir o valor é aceito;
    mostrar um valor ERRADO, não — isso ensina que fib(0) devolve 8.
    """
    def checa(r, d):
        for s in d["steps"]:
            if s.get("event") != "return" or "retval" not in s:
                continue
            topo = s["stack"][-1] if s["stack"] else {}
            v = (topo.get("vars") or {}).get(param)
            if not v or "text" not in v or not v["text"].lstrip("-").isdigit():
                continue
            n = int(v["text"])
            texto = s["retval"].get("text", "")
            if not texto.lstrip("-").isdigit():
                continue
            if 0 <= n < len(FIB) and FIB[n] != int(texto):
                return "quadro com %s=%d diz devolveu %s (fib=%d)" % (param, n, texto, FIB[n])
        return None
    return checa


def ciclo(nome):
    """A caixa que `nome` alcança aponta para SI MESMA.

    O exemplo "py ciclo" existia e verificava só o stdout — ou seja, verificava
    que o programa roda, não que o DESENHO diz o que ele existe para dizer. Um
    ciclo que deixasse de se fechar (ou que fizesse a expansão não terminar)
    passava por aqui sem ninguém notar."""
    def checa(r, d):
        for passo in reversed(d["steps"]):
            for rid in _refs_de(passo, nome):
                b = _caixas(passo).get(rid) or {}
                for v in (b.get("fields") or {}).values():
                    if isinstance(v, dict) and v.get("ref") == rid:
                        return None
        return "nenhuma caixa de %s aponta para si mesma" % nome
    return checa


def devolveu(esperado, proc=None):
    """Algum passo de retorno mostrou EXATAMENTE este valor.

    `retorno_nunca_mente` só reprova um valor errado; ele passa de graça quando
    valor nenhum é mostrado — e foi exatamente isso que aconteceu com o Odin
    quando os retornos ambíguos passaram a ser omitidos: o verificador que pegou
    o bug do fibonacci virou vacuamente verdadeiro. Este aqui cobra o caso
    POSITIVO, que é o que dá sentido ao outro."""
    def checa(r, d):
        vistos = []
        for s in d["steps"]:
            if "retval" not in s:
                continue
            topo = s["stack"][-1] if s["stack"] else {}
            if proc and topo.get("func") != proc:
                continue
            texto = s["retval"].get("text")
            vistos.append(texto)
            if texto == esperado:
                return None
        return "nenhum `devolveu` %s (vi %s)" % (esperado, vistos or "nenhum")
    return checa


def saida_acompanha_os_passos():
    """`stdout_len` tem de crescer ao longo do trace e terminar no tamanho da
    saída de verdade.

    O driver do Odin mandava 0 em TODO passo, e o painel "Saída até aqui" dizia
    "(sem saída ainda)" em todos eles — inclusive no último, de um programa que
    imprimiu. Nada verificava isso porque nada olhava o campo."""
    def checa(r, d):
        lens = [s.get("stdout_len", 0) for s in d["steps"]]
        if not lens:
            return "nenhum passo"
        if lens != sorted(lens):
            return "stdout_len não é monótono: %s" % lens[:12]
        total = len(r.get("stdout") or "")
        if total and lens[-1] == 0:
            return "programa imprimiu %d caracteres e todo passo diz 0" % total
        if lens[-1] > total:
            return "stdout_len final %d passa da saída real %d" % (lens[-1], total)
        return None
    return checa


def sem_alias_falso(a, b, tam_a, tam_b):
    """`a` e `b` compartilham o começo do arranjo mas têm comprimentos
    diferentes: são caixas DIFERENTES, cada uma com o seu tamanho.

    Com a chave só no ponteiro de dados as duas caíam na mesma caixa, e o
    diagrama desenhava duas setas para uma caixa de `tam_a` itens — ensinando que
    `b` é a mesma fatia de `a`, contra o `len(b)` do próprio programa."""
    def checa(r, d):
        for passo in reversed(d["steps"]):
            ra, rb = _refs_de(passo, a), _refs_de(passo, b)
            if not (ra and rb):
                continue
            if ra[0] == rb[0]:
                return "%s e %s colapsaram na caixa %s" % (a, b, ra[0])
            ca, cb = _caixas(passo).get(ra[0]) or {}, _caixas(passo).get(rb[0]) or {}
            na, nb = len(ca.get("items") or []), len(cb.get("items") or [])
            if na != tam_a or nb != tam_b:
                return "esperava %d e %d itens, vi %d e %d" % (tam_a, tam_b, na, nb)
            return None
        return "nenhum passo teve %s e %s juntos" % (a, b)
    return checa


def caixa_com_campo(cls, campo):
    def checa(r, d):
        for passo in d["steps"]:
            for b in _caixas(passo).values():
                if b.get("cls") == cls and campo in (b.get("fields") or {}):
                    return None
        return "nenhuma caixa %s com o campo %s" % (cls, campo)
    return checa


def crash_diz(trecho):
    def checa(r, d):
        c = d.get("crash") or {}
        msg = "%s %s" % (c.get("type", ""), c.get("message", ""))
        return None if trecho.lower() in msg.lower() else "quebra diz %r, esperava conter %r" % (msg, trecho)
    return checa


# ── o corpus ─────────────────────────────────────────────────────────────────

PY_ALIAS = '''notas = [7, 8, 9]
copia = notas
outra = [7, 8, 9]
notas.append(6)
print(len(notas), len(copia), len(outra))
'''

PY_FIB = '''def fibonacci(n):
    if n < 1:
        return 0
    if n == 1:
        return 1
    return fibonacci(n - 1) + fibonacci(n - 2)

print(fibonacci(6))
'''

PY_OBJ = '''class Aluno:
    def __init__(self, nome, notas):
        self.nome = nome
        self.notas = notas

a = Aluno("Ana", [7, 8])
b = a
print(a.nome, b.nome)
'''

PY_CICLO = '''class No:
    def __init__(self):
        self.prox = None

n = No()
n.prox = n
print(n.prox is n)
'''

PY_QUEBRA = '''def divide(a, b):
    return a / b

print(divide(10, 0))
'''

OD_ALIAS = '''package main
import "core:fmt"
main :: proc() {
    notas := []int{7, 8, 9}
    copia := notas
    outra := []int{7, 8, 9}
    fmt.println(len(notas), len(copia), len(outra))
}
'''

OD_FIB = '''package main
import "core:fmt"
fibonacci :: proc(n: int) -> int {
    switch {
    case n < 1: return 0
    case n == 1: return 1
    }
    return fibonacci(n - 1) + fibonacci(n - 2)
}
main :: proc() { fmt.println(fibonacci(6)) }
'''

OD_STRUCT = '''package main
import "core:fmt"
User :: struct { username: string, age: int }
valida :: proc(u: User) -> bool { return len(u.username) >= 3 }
main :: proc() {
    u := User{username = "jo", age = 16}
    fmt.println(valida(u))
}
'''

OD_LISTA = '''package main
import "core:fmt"
Node :: struct { valor: int, prox: ^Node }
main :: proc() {
    cabeca: ^Node = nil
    for i in 1..=4 {
        n := new(Node); n.valor = i; n.prox = cabeca; cabeca = n
    }
    fmt.println(cabeca.valor)
}
'''

OD_VAZIOS = '''package main
import "core:fmt"
main :: proc() {
    a: [dynamic]int
    b: [dynamic]int
    append(&a, 1)
    fmt.println(len(a), len(b))
}
'''

OD_QUEBRA = '''package main
import "core:fmt"
main :: proc() {
    xs := []int{1, 2, 3}
    i := 10
    fmt.println(xs[i])
}
'''

OD_CHAMADA = '''package main
import "core:fmt"
dobro :: proc(n: int) -> int { return n * 2 }
main :: proc() { fmt.println(dobro(21)) }
'''

# Uma sub-fatia sobre o MESMO arranjo: `parte` começa onde `todos` começa e tem
# comprimento menor. As duas eram a mesma caixa, e o desenho dizia que `parte`
# tem 3 itens.
OD_SUBFATIA = '''package main
import "core:fmt"
main :: proc() {
    todos := []int{10, 20, 30}
    parte := todos[:2]
    fmt.println(len(todos), len(parte))
}
'''

PY_CLOSURE = '''def contador():
    n = 0
    def incr():
        return n + 1
    return incr

f = contador()
print(f())
'''

CORPUS = [
    ("py alias + mutação", "python", PY_ALIAS,
     [stdout_e("4 4 3\n"), alias("notas", "copia"), nao_alias("notas", "outra"),
      id_estavel("notas"), saida_acompanha_os_passos()]),
    ("py fibonacci recursivo", "python", PY_FIB,
     [stdout_e("8\n"), eventos({"call": 2, "return": 2}), retorno_nunca_mente(),
      devolveu("8", proc="fibonacci")]),
    ("py objeto + alias", "python", PY_OBJ,
     [stdout_e("Ana Ana\n"), alias("a", "b"), caixa_com_campo("Aluno", "nome")]),
    # O ciclo agora é VERIFICADO, e não só executado.
    ("py ciclo", "python", PY_CICLO, [stdout_e("True\n"), ciclo("n")]),
    ("py closure", "python", PY_CLOSURE, [stdout_e("1\n"), eventos({"return": 2})]),
    ("py quebra", "python", PY_QUEBRA,
     [status_e("runtime_error"), crash_diz("ZeroDivisionError")]),

    ("odin alias + fatia", "odin", OD_ALIAS,
     [stdout_e("3 3 3\n"), alias("notas", "copia"), nao_alias("notas", "outra"),
      saida_acompanha_os_passos()]),
    ("odin fibonacci recursivo", "odin", OD_FIB,
     [stdout_e("8\n"), eventos({"call": 2, "return": 2}), retorno_nunca_mente()]),
    ("odin struct por valor", "odin", OD_STRUCT,
     [stdout_e("false\n"), caixa_com_campo("User", "username")]),
    ("odin lista ligada (new)", "odin", OD_LISTA,
     [stdout_e("4\n"), cadeia_de(4)]),
    ("odin dois vazios distintos", "odin", OD_VAZIOS,
     [stdout_e("1 0\n"), nao_alias("a", "b")]),
    # E o inverso do anterior: o ponteiro de dados IGUAL, o comprimento diferente.
    ("odin sub-fatia", "odin", OD_SUBFATIA,
     [stdout_e("3 2\n"), sem_alias_falso("todos", "parte", 3, 2)]),
    ("odin quebra", "odin", OD_QUEBRA,
     [status_e("runtime_error"), crash_diz("out of range")]),
    # O caso POSITIVO do `devolveu`: sem ele, `retorno_nunca_mente` passa de graça
    # num trace que não mostra retorno nenhum.
    ("odin chamada simples", "odin", OD_CHAMADA,
     [stdout_e("42\n"), eventos({"return": 1}), devolveu("42", proc="dobro")]),
]


def main():
    falhas = 0
    for rot, lang, src, perguntas in CORPUS:
        try:
            r = trace(lang, src)
        except Exception as e:
            print("%-32s ERRO NA CHAMADA: %s" % (rot, e))
            falhas += 1
            continue
        d = remonta_heaps(json.loads(r["trace_report"])) if r.get("trace_report") else {"steps": []}
        problemas = [p for p in (q(r, d) for q in perguntas) if p]
        marca = "ok " if not problemas else "FALHOU"
        print("%-32s %-6s passos=%-4d %s" % (rot, marca, len(d.get("steps", [])),
                                             "; ".join(problemas)))
        falhas += bool(problemas)
    print("\n%d de %d exemplos passaram." % (len(CORPUS) - falhas, len(CORPUS)))
    return 1 if falhas else 0


if __name__ == "__main__":
    sys.exit(main())
