#!/usr/bin/env bash
#
# smoke-escape.sh — adversarial corpus run against a LIVE, nsjail-backed runner.
#
# Every submission below MUST be contained by the per-run jail, and the runner
# itself MUST survive all of them (no daemon crash, no host OOM). The script
# fails (non-zero) the moment any containment expectation is not met, and ends
# with a host-survival check: a normal run must still succeed afterwards.
#
# It deliberately does NOT weaken the jail — it only asserts the containment the
# jail already provides. See docs/RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md §4.4.
#
# Env:
#   RUNNER_URL             base URL of the runner   (default http://localhost:8090)
#   RUNNER_SERVICE_TOKEN   sent as X-Runner-Token   (optional; dev boot needs none)
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

# post <source> -> prints the raw JSON RunResult on stdout.
post() {
  local src="$1"
  local body
  body="$(jq -nc --arg src "$src" '{language: "python", source_code: $src}')"
  curl -fsS "${BASE}/run" \
    -H 'content-type: application/json' \
    "${auth[@]}" \
    -d "$body"
}

fail() { echo "ESCAPE FAIL [$1]: $2" >&2; echo "  resp: ${3:-<none>}" >&2; exit 1; }

# jqtrue <json> <filter> -> success iff the filter is truthy.
jqtrue() { echo "$1" | jq -e "$2" >/dev/null 2>&1; }

echo "== escape corpus =="

# ---------------------------------------------------------------------------
# 1) Fork bomb -> contained by --rlimit_nproc / container pids cap; host lives.
#    We don't care what status it ends in, only that the daemon answered (it
#    was not taken down) and that the run did not report a clean success.
# ---------------------------------------------------------------------------
resp="$(post $'import os\nwhile True:\n    os.fork()' || true)"
[ -n "$resp" ] || fail forkbomb "runner did not respond — daemon may have been taken down" "$resp"
if jqtrue "$resp" '.status == "success"'; then
  fail forkbomb "fork bomb reported success — not contained" "$resp"
fi
echo "  [1] fork bomb contained (status=$(echo "$resp" | jq -r .status))"

# ---------------------------------------------------------------------------
# 2) Secret read + host-env leak -> denied by read-only jail + minimal env.
#    /etc/shadow is owned by an uid NOT mapped into the jail's userns, so the
#    jail's namespace-root cannot read it (EACCES); os.environ must carry only
#    the two vars the runtime sets.
# ---------------------------------------------------------------------------
read -r -d '' SECRET <<'PY' || true
import os
shadow = "readable"
try:
    with open("/etc/shadow") as f:
        f.read()
except OSError:
    shadow = "denied"
allowed = {"PATH", "PYTHONUNBUFFERED"}
leaked = sorted(k for k in os.environ if k not in allowed)
print("shadow=" + shadow)
print("leaked=" + (",".join(leaked) or "none"))
PY
resp="$(post "$SECRET" || true)"
jqtrue "$resp" '.status == "success"'                 || fail secret "run did not complete cleanly" "$resp"
jqtrue "$resp" '.stdout | test("shadow=denied")'      || fail secret "/etc/shadow was READABLE inside the jail" "$resp"
jqtrue "$resp" '.stdout | test("leaked=none")'        || fail secret "host environment leaked into the jail" "$resp"
echo "  [2] secret read denied + env minimal"

# ---------------------------------------------------------------------------
# 3) Dangerous syscall -> killed by the nsjail seccomp denylist (SIGSYS).
#    unshare(CLONE_NEWUSER) is on the KILL list; the process must die before
#    printing SURVIVED.
# ---------------------------------------------------------------------------
read -r -d '' SYSCALL <<'PY' || true
import ctypes
libc = ctypes.CDLL(None, use_errno=True)
libc.unshare(0x10000000)  # CLONE_NEWUSER — on the seccomp KILL list
print("SURVIVED")
PY
resp="$(post "$SYSCALL" || true)"
[ -n "$resp" ] || fail syscall "runner did not respond" "$resp"
jqtrue "$resp" '.status != "success"'                 || fail syscall "dangerous syscall was not blocked" "$resp"
jqtrue "$resp" '.stdout | test("SURVIVED") | not'     || fail syscall "process survived the killed syscall" "$resp"
echo "  [3] dangerous syscall killed by seccomp (status=$(echo "$resp" | jq -r .status))"

# ---------------------------------------------------------------------------
# 4) Output flood -> captured stdout truncated at the cap, no runner OOM.
#    Streams far past RUNNER_MAX_OUTPUT_BYTES; the limitedBuffer discards the
#    overflow (never buffers it), so the daemon neither OOMs nor hangs.
# ---------------------------------------------------------------------------
resp="$(post $'for _ in range(10_000_000):\n    print("x" * 64)' || true)"
[ -n "$resp" ] || fail flood "runner did not respond — possible OOM/hang" "$resp"
jqtrue "$resp" '.stdout | test("\\[output truncated\\]")' || fail flood "output was not truncated at the cap" "$resp"
echo "  [4] output flood truncated, no OOM (status=$(echo "$resp" | jq -r .status))"

# ---------------------------------------------------------------------------
# Host-survival gate: after the whole corpus, a normal run must still succeed.
# ---------------------------------------------------------------------------
echo "== host survival =="
RUNNER_URL="$BASE" "$here/smoke-run.sh" python 'print(2+2)' $'4\n'
echo "ESCAPE CORPUS PASSED — every submission contained, host survived."
