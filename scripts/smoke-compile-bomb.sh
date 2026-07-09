#!/usr/bin/env bash
#
# smoke-compile-bomb.sh — the §4.4 "compiler bomb" corpus row (F-D).
#
# The compiler is untrusted input too, so the compile phase is jailed with its own
# CPU/memory/time limits. This submits a preprocessor token-multiplication bomb
# (10^6 array elements — NOT memoised the way C++ templates are) with a short
# compile_timeout_ms and asserts it is contained as `compile_error`, that nothing
# is executed, and that the runner survives (a normal run still succeeds after).
#
# It does NOT weaken anything — it only asserts the containment the compile jail
# already provides.
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

# A source small enough to accept, but whose preprocessed form explodes to ~10^6
# array elements — gcc blows the compile timeout (or the compile memory cap).
read -r -d '' BOMB <<'C' || true
#define C0 1,1,1,1,1,1,1,1,1,1
#define C1 C0,C0,C0,C0,C0,C0,C0,C0,C0,C0
#define C2 C1,C1,C1,C1,C1,C1,C1,C1,C1,C1
#define C3 C2,C2,C2,C2,C2,C2,C2,C2,C2,C2
#define C4 C3,C3,C3,C3,C3,C3,C3,C3,C3,C3
#define C5 C4,C4,C4,C4,C4,C4,C4,C4,C4,C4
#define C6 C5,C5,C5,C5,C5,C5,C5,C5,C5,C5
int a[] = { C6 };
int main(void){ return 0; }
C

body="$(jq -nc --arg src "$BOMB" '{language: "c", source_code: $src, compile_timeout_ms: 2000}')"
resp="$(curl -fsS "${BASE}/run" -H 'content-type: application/json' "${auth[@]}" -d "$body" || true)"

[ -n "$resp" ] || { echo "COMPILE-BOMB FAIL: runner did not respond — possible hang/OOM" >&2; exit 1; }

status="$(echo "$resp" | jq -r .status)"
stdout="$(echo "$resp" | jq -r .stdout)"
if [ "$status" != "compile_error" ]; then
  echo "COMPILE-BOMB FAIL: expected compile_error, got status=$status" >&2
  echo "  resp: $resp" >&2
  exit 1
fi
if [ -n "$stdout" ]; then
  echo "COMPILE-BOMB FAIL: a contained compile must not run anything, stdout=$stdout" >&2
  exit 1
fi
echo "  compiler bomb contained (status=compile_error)"

# Host-survival gate: a normal C build+run must still succeed afterwards.
RUNNER_URL="$BASE" "$here/smoke-run.sh" c $'#include <stdio.h>\nint main(void){ puts("4"); return 0; }' $'4\n'
echo "COMPILE-BOMB PASSED — bomb contained, runner survived."
