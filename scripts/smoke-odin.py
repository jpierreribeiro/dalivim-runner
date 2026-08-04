"""Smoke on-target da B.1 (Odin) contra o runner de verdade, dentro da imagem.

Cobre o que docs/future/ODIN.md deixou em aberto: success, memory_exceeded,
timeout, egress contido — mais compile_error (diagnóstico tem de chegar ao aluno)
e a prova das escolhas de postura do odinSpec.
"""
import json
import os
import urllib.error
import urllib.request

TOKEN = os.environ.get("RUNNER_SERVICE_TOKEN", "")
URL = "http://127.0.0.1:8090/run"


def call(payload, timeout=200):
    req = urllib.request.Request(
        URL,
        data=json.dumps(payload).encode(),
        headers=dict({"content-type": "application/json"},
                     **({"X-Runner-Token": TOKEN} if TOKEN else {})),
    )
    try:
        return json.load(urllib.request.urlopen(req, timeout=timeout))
    except urllib.error.HTTPError as e:
        return {"status": "HTTP %d" % e.code, "stderr": e.read()[:200].decode()}


HELLO = 'package main\n\nimport "core:fmt"\n\nmain :: proc() {\n\tfmt.println(6 * 7)\n}\n'

# Aloca em blocos até o teto do cgroup (memory_mb) — o kernel OOM classifica.
BOMB = (
    'package main\n\n'
    'import "core:fmt"\n\n'
    'main :: proc() {\n'
    '\ttotal := 0\n'
    '\tfor i in 0 ..< 100000 {\n'
    '\t\tbuf := make([]u8, 8 * 1024 * 1024)\n'
    '\t\tfor j := 0; j < len(buf); j += 4096 {\n'
    '\t\t\tbuf[j] = u8(i)\n'
    '\t\t}\n'
    '\t\ttotal += len(buf)\n'
    '\t}\n'
    '\tfmt.println(total)\n'
    '}\n'
)

SPIN = (
    'package main\n\n'
    'import "core:fmt"\n\n'
    'main :: proc() {\n'
    '\tx := 0\n'
    '\tfor {\n'
    '\t\tx += 1\n'
    '\t\tif x < 0 {\n'
    '\t\t\tfmt.println(x)\n'
    '\t\t}\n'
    '\t}\n'
    '}\n'
)

# Egress: resolver/discar para fora tem de falhar no netns vazio do jail.
EGRESS = (
    'package main\n\n'
    'import "core:fmt"\n'
    'import "core:net"\n\n'
    'main :: proc() {\n'
    '\tep := net.Endpoint{address = net.IP4_Address{1, 1, 1, 1}, port = 53}\n'
    '\tsock, err := net.dial_tcp_from_endpoint(ep)\n'
    '\tif err != nil {\n'
    '\t\tfmt.println("egress-blocked")\n'
    '\t\treturn\n'
    '\t}\n'
    '\tnet.close(sock)\n'
    '\tfmt.println("EGRESS-ABERTO")\n'
    '}\n'
)

BAD = 'package main\n\nmain :: proc() {\n\tx : int = "nao sou int"\n}\n'

CASES = [
    ("success", {"language": "odin", "source_code": HELLO}, "success"),
    ("compile_error", {"language": "odin", "source_code": BAD}, "compile_error"),
    ("memory_exceeded", {"language": "odin", "source_code": BOMB, "memory_mb": 128}, "memory_exceeded"),
    ("timeout", {"language": "odin", "source_code": SPIN, "timeout_ms": 2000}, "timeout"),
    ("egress contido", {"language": "odin", "source_code": EGRESS}, "success"),
]

falhas = 0
for name, payload, want in CASES:
    o = call(payload)
    got = o.get("status")
    detalhe = (o.get("stdout") or "").strip() or (o.get("stderr") or o.get("compile_output") or "").strip()
    ok = got == want
    if name == "egress contido":
        ok = ok and "egress-blocked" in (o.get("stdout") or "")
    if name == "compile_error":
        ok = ok and "Error" in (o.get("compile_output") or "")
    if not ok:
        falhas += 1
    print(
        "%-16s %-16s (esperado %-16s) %s  %s"
        % (name, got, want, "OK" if ok else "FALHOU", detalhe.replace("\n", " ")[:70])
    )

v = call({"language": "odin", "source_code": HELLO})
print("\nruntime_version reportado:", repr(v.get("runtime_version")))
print("compile_ms=%s duration_ms=%s memory_kb=%s" % (v.get("compile_ms"), v.get("duration_ms"), v.get("memory_kb")))


# ── B.2: o PASSO A PASSO, contra um gdb de verdade ──────────────────────────
#
# Isto não existia, e a ausência importava mais do que parece: TODAS as garantias
# do driver do Odin eram (a) `strings.Contains` sobre o fonte dele, ou (b) o
# driver rodando contra um gdb FALSO. Nenhuma das duas toca DWARF, prólogo,
# convenção de chamada ou ASLR — que é justamente onde as decisões do driver
# foram tomadas, e onde as medições do handoff foram feitas à mão.
#
# Aqui é o gdb de verdade sobre um binário de verdade. Cada verificação abaixo
# corresponde a um defeito que já chegou a produção, ou a uma correção cuja
# validação on-target estava explicitamente pendente.

TRACE_DOBRO = (
    'package main\n\n'
    'import "core:fmt"\n\n'
    'dobro :: proc(n: int) -> int {\n'
    '\treturn n * 2\n'
    '}\n\n'
    'main :: proc() {\n'
    '\tfmt.println(dobro(21))\n'
    '}\n'
)

# `f(n-1) + f(n-2)`: duas chamadas na MESMA linha. É o programa em que a
# atribuição do valor devolvido por profundidade ou por ordem de disparo produziu
# `n = 0` dizendo `devolveu 8`.
TRACE_FIB = (
    'package main\n\n'
    'import "core:fmt"\n\n'
    'fibonacci :: proc(n: int) -> int {\n'
    '\tswitch {\n'
    '\tcase n < 1: return 0\n'
    '\tcase n == 1: return 1\n'
    '\t}\n'
    '\treturn fibonacci(n - 1) + fibonacci(n - 2)\n'
    '}\n\n'
    'main :: proc() {\n'
    '\tfmt.println(fibonacci(6))\n'
    '}\n'
)

# Mesmo ponteiro de dados, comprimentos diferentes.
TRACE_SUBFATIA = (
    'package main\n\n'
    'import "core:fmt"\n\n'
    'main :: proc() {\n'
    '\ttodos := []int{10, 20, 30}\n'
    '\tparte := todos[:2]\n'
    '\tfmt.println(len(todos), len(parte))\n'
    '}\n'
)


def remonta(doc):
    """O heap CHEIO de cada passo, remontando os deltas — a mesma passada que o
    frontend faz. Conferir o fio em vez do que o aluno vê seria outra coisa."""
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
    o = call(dict({"language": "odin", "mode": "trace", "source_code": src}, **extra))
    doc = json.loads(o["trace_report"]) if o.get("trace_report") else {"steps": []}
    return o, doc, remonta(doc)


def cheque(nome, cond, detalhe=""):
    global falhas
    if not cond:
        falhas += 1
    print("%-46s %s  %s" % (nome, "OK" if cond else "FALHOU", str(detalhe)[:80]))


print("\n── B.2: passo a passo sob gdb ──")

# 1. O básico, e o formato.
o, doc, heaps = tracar(TRACE_DOBRO)
cheque("trace: status success", o.get("status") == "success", o.get("status"))
cheque("trace: formato @2", o.get("trace_format") == "dalivim-trace-json@2", o.get("trace_format"))
cheque("trace: passos gravados", len(doc.get("steps", [])) > 0, len(doc.get("steps", [])))
cheque("trace: stdout do ALUNO, sem o gdb", (o.get("stdout") or "") == "42\n", repr(o.get("stdout")))

# 2. O heap em delta (item 3 das pendências), medido no fio.
cheque("trace: heap_encoding=delta", doc.get("heap_encoding") == "delta", doc.get("heap_encoding"))
cheios = [s for s in doc.get("steps", []) if "heap" in s]
cheque("trace: um único passo com heap cheio", len(cheios) <= 1, len(cheios))

# 3. stdout_len — era 0 em TODO passo, e o painel "Saída até aqui" ficava vazio.
lens = [s.get("stdout_len", 0) for s in doc.get("steps", [])]
cheque("trace: stdout_len é monótono", lens == sorted(lens), lens[:10])
cheque("trace: stdout_len chega na saída real", lens and lens[-1] == len(o.get("stdout") or ""),
       "%s vs %d" % (lens[-1] if lens else None, len(o.get("stdout") or "")))

# 4. O prólogo: o passo de `call` não pode afirmar que a proc não tem parâmetros.
entrou = [s for s in doc.get("steps", [])
          if s.get("event") == "call" and s["stack"] and s["stack"][-1].get("func") == "dobro"]
cheque("trace: passo de call em dobro", bool(entrou), len(entrou))
if entrou:
    topo = entrou[0]["stack"][-1]
    cheque("trace: prólogo marcado quando esconde argumento",
           bool(topo.get("vars")) or topo.get("prologo") is True, topo)

# 5. `devolveu 42` — o caso positivo, que o corpus não cobria.
devolvidos = [s["retval"].get("text") for s in doc.get("steps", []) if "retval" in s]
cheque("trace: dobro(21) devolveu 42", "42" in devolvidos, devolvidos)

# 6. RECURSÃO — a validação on-target que a quarta abordagem de 5.1 esperava.
#    A regra dura: um valor MOSTRADO tem de bater com o parâmetro do quadro.
#    Omitir é aceito; mentir não é, porque ensina que fib(0) devolve 8.
FIB = [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]
o2, doc2, _ = tracar(TRACE_FIB, timeout_ms=30000)
cheque("trace fib: status success", o2.get("status") == "success", o2.get("status"))
cheque("trace fib: saída 8", (o2.get("stdout") or "").strip() == "8", repr(o2.get("stdout")))
mentiras, mostrados = [], 0
for s in doc2.get("steps", []):
    if "retval" not in s or not s.get("stack"):
        continue
    var = (s["stack"][-1].get("vars") or {}).get("n") or {}
    txt, val = s["retval"].get("text", ""), var.get("text", "")
    if not (val.lstrip("-").isdigit() and txt.lstrip("-").isdigit()):
        continue
    mostrados += 1
    n = int(val)
    if 0 <= n < len(FIB) and FIB[n] != int(txt):
        mentiras.append("n=%d diz devolveu %s (fib=%d)" % (n, txt, FIB[n]))
cheque("trace fib: RETORNO NUNCA MENTE", not mentiras, "; ".join(mentiras))
# Informativo, não reprova: se a chave de quadro não resolver on-target, o driver
# cai na regra conservadora e simplesmente omite — que é o comportamento de antes.
print("%-46s %d quadros recursivos mostraram `devolveu`" % ("(informativo)", mostrados))

# 7. Sub-fatia: mesmo `data`, comprimentos diferentes, caixas diferentes.
o3, doc3, heaps3 = tracar(TRACE_SUBFATIA)
cheque("trace sub-fatia: saída 3 2", (o3.get("stdout") or "").strip() == "3 2", repr(o3.get("stdout")))
achou = False
for i, s in enumerate(doc3.get("steps", [])):
    v = (s["stack"][-1].get("vars") or {}) if s.get("stack") else {}
    ra, rb = (v.get("todos") or {}).get("ref"), (v.get("parte") or {}).get("ref")
    if not (ra and rb):
        continue
    achou = True
    ca, cb = heaps3[i].get(ra) or {}, heaps3[i].get(rb) or {}
    cheque("trace sub-fatia: caixas distintas", ra != rb, "%s vs %s" % (ra, rb))
    cheque("trace sub-fatia: 3 e 2 itens",
           len(ca.get("items") or []) == 3 and len(cb.get("items") or []) == 2,
           "%d e %d" % (len(ca.get("items") or []), len(cb.get("items") or [])))
    break
cheque("trace sub-fatia: passo com as duas fatias", achou)

# 8. A quebra continua classificada como no modo run, e diz O QUÊ.
o4, doc4, _ = tracar(
    'package main\n\nimport "core:fmt"\n\nmain :: proc() {\n'
    '\txs := []int{1, 2, 3}\n\ti := 10\n\tfmt.println(xs[i])\n}\n')
cheque("trace quebra: runtime_error como no modo run", o4.get("status") == "runtime_error", o4.get("status"))
crash = doc4.get("crash") or {}
cheque("trace quebra: diz o quê, não o sinal",
       "out of range" in (crash.get("message") or "").lower(),
       "%s: %s" % (crash.get("type"), crash.get("message")))

raise SystemExit(1 if falhas else 0)
