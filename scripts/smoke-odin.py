"""Smoke on-target da B.1 (Odin) contra o runner de verdade, dentro da imagem.

Cobre o que docs/future/ODIN.md deixou em aberto: success, memory_exceeded,
timeout, egress contido — mais compile_error (diagnóstico tem de chegar ao aluno)
e a prova das escolhas de postura do odinSpec.
"""
import json
import os
import urllib.error
import urllib.request

TOKEN = os.environ["RUNNER_SERVICE_TOKEN"]
URL = "http://127.0.0.1:8090/run"


def call(payload, timeout=200):
    req = urllib.request.Request(
        URL,
        data=json.dumps(payload).encode(),
        headers={"content-type": "application/json", "X-Runner-Token": TOKEN},
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
raise SystemExit(1 if falhas else 0)
