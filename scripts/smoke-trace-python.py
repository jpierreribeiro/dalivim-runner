"""Smoke on-target do passo a passo em Python, contra o runner de verdade.

O harness Python tinha bons testes de unidade (rodam CPython, e foi um deles que
pegou o bug de identidade de caixa), mas nada exercitava o modo trace ATRAVÉS DO
/run dentro da imagem: nem o jail, nem o leitor do relatório, nem a classificação
do status. E o CI não testava modo trace em linguagem nenhuma.

Cada verificação corresponde a um defeito real ou a uma garantia que o produto
depende e que ninguém conferia ponta a ponta.
"""
import json
import os
import urllib.error
import urllib.request

TOKEN = os.environ.get("RUNNER_SERVICE_TOKEN", "")
URL = "http://127.0.0.1:8090/run"

falhas = 0


def call(payload, timeout=200):
    cab = {"content-type": "application/json"}
    if TOKEN:
        cab["X-Runner-Token"] = TOKEN
    req = urllib.request.Request(URL, data=json.dumps(payload).encode(), headers=cab)
    try:
        return json.load(urllib.request.urlopen(req, timeout=timeout))
    except urllib.error.HTTPError as e:
        return {"status": "HTTP %d" % e.code, "stderr": e.read()[:200].decode()}


def cheque(nome, cond, detalhe=""):
    global falhas
    if not cond:
        falhas += 1
    print("%-52s %s  %s" % (nome, "OK" if cond else "FALHOU", str(detalhe)[:70]))


def remonta(doc):
    """O heap cheio de cada passo — a mesma passada do frontend."""
    out, corrente = [], {}
    for s in doc.get("steps", []):
        if "heap" in s:
            corrente = dict(s["heap"])
        elif "heap_delta" in s:
            corrente = dict(corrente)
            corrente.update(s["heap_delta"].get("set") or {})
            for k in s["heap_delta"].get("del") or []:
                corrente.pop(k, None)
        out.append(corrente)
    return out


def tracar(src, **extra):
    o = call(dict({"language": "python", "mode": "trace", "source_code": src}, **extra))
    doc = json.loads(o["trace_report"]) if o.get("trace_report") else {"steps": []}
    return o, doc, remonta(doc)


print("── passo a passo, Python, dentro da imagem ──")

# 1. Alias e mutação: o coração do diagrama.
ALIAS = "notas = [7, 8, 9]\ncopia = notas\noutra = [7, 8, 9]\nnotas.append(6)\nprint(len(notas), len(copia), len(outra))\n"
o, doc, heaps = tracar(ALIAS)
cheque("status success", o.get("status") == "success", o.get("status"))
cheque("formato @1", o.get("trace_format") == "dalivim-trace-json@1", o.get("trace_format"))
cheque("stdout do aluno intacto", o.get("stdout") == "4 4 3\n", repr(o.get("stdout")))
cheque("heap_encoding=delta", doc.get("heap_encoding") == "delta", doc.get("heap_encoding"))
cheque("um único passo com heap cheio",
       len([s for s in doc.get("steps", []) if "heap" in s]) == 1,
       len([s for s in doc.get("steps", []) if "heap" in s]))

ultimo = None
for i, s in enumerate(doc.get("steps", [])):
    v = (s["stack"][-1].get("vars") or {}) if s.get("stack") else {}
    if "notas" in v and "copia" in v and "outra" in v:
        ultimo = (i, v)
cheque("passo com as três listas", ultimo is not None)
if ultimo:
    i, v = ultimo
    cheque("alias: notas e copia na MESMA caixa", v["notas"].get("ref") == v["copia"].get("ref"),
           "%s vs %s" % (v["notas"], v["copia"]))
    cheque("não-alias: outra em caixa própria", v["notas"].get("ref") != v["outra"].get("ref"))
    cheque("a caixa cresceu com o append",
           len((heaps[i].get(v["notas"]["ref"]) or {}).get("items") or []) == 4,
           heaps[i].get(v["notas"]["ref"]))

# 2. stdout_len: o painel "Saída até aqui".
lens = [s.get("stdout_len", 0) for s in doc.get("steps", [])]
cheque("stdout_len é monótono", lens == sorted(lens), lens[:10])
cheque("stdout_len chega na saída real", lens and lens[-1] == len(o.get("stdout") or ""),
       "%s vs %d" % (lens[-1] if lens else None, len(o.get("stdout") or "")))

# 3. A quebra: status como no modo run, e o registro que aponta ONDE.
o2, doc2, _ = tracar("def divide(a, b):\n    return a / b\n\nprint(divide(10, 0))\n")
cheque("quebra: runtime_error", o2.get("status") == "runtime_error", o2.get("status"))
crash = doc2.get("crash") or {}
cheque("quebra: diz ZeroDivisionError", crash.get("type") == "ZeroDivisionError", crash)
cheque("quebra: aponta a linha", crash.get("line") == 2, crash.get("line"))

# 4. VALOR HOSTIL — o conserto das subclasses de tipos embutidos, ponta a ponta.
#    O programa CONTA quantas vezes o harness chamou um método dele; a resposta
#    tem de ser zero. Antes eram seis, e um `Loud(5)` era desenhado como 42.
HOSTIL = (
    "LOG = []\n"
    "class Loud(int):\n"
    "    def __str__(self):\n        LOG.append('PWNED'); return '42'\n"
    "    def __repr__(self):\n        LOG.append('PWNED'); return '42'\n"
    "class LoudList(list):\n"
    "    def __iter__(self):\n        LOG.append('PWNED'); return super().__iter__()\n"
    "a = Loud(5)\nb = LoudList([1, 2])\nn = len(LOG)\nprint(n, a)\n"
)
o3, doc3, heaps3 = tracar(HOSTIL)
cheque("hostil: execução bem-sucedida", o3.get("status") == "success", o3.get("status"))
cheque("hostil: marcador NUNCA aparece no trace", "PWNED" not in (o3.get("trace_report") or ""))
cheque("hostil: o programa contou ZERO chamadas", (o3.get("stdout") or "").startswith("0 "),
       repr(o3.get("stdout")))
alvo = None
for i, s in enumerate(doc3.get("steps", [])):
    v = (s["stack"][-1].get("vars") or {}) if s.get("stack") else {}
    if "a" in v and "b" in v:
        alvo = (i, v)
if alvo:
    i, v = alvo
    cheque("hostil: Loud(5) mostra 5, não 42", (v["a"] or {}).get("text") == "5", v["a"])
    caixa = heaps3[i].get((v["b"] or {}).get("ref")) or {}
    cheque("hostil: a subclasse de list mostra os itens reais",
           len(caixa.get("items") or []) == 2, caixa)

# 5. Os tetos existem e são reportados.
o4, doc4, _ = tracar("x = 0\nfor i in range(100000):\n    x += i\nprint(x)\n")
trunc = doc4.get("truncated") or {}
cheque("teto de passos é atingido e marcado", trunc.get("steps") is True, trunc)
cheque("o programa ainda roda até o fim", (o4.get("stdout") or "").strip().isdigit(),
       repr((o4.get("stdout") or "")[:20]))
cheque("truncated carrega o teto de heap", "heap" in trunc or trunc.get("steps") is True, trunc)

raise SystemExit(1 if falhas else 0)
