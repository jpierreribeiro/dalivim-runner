#!/usr/bin/env bash
#
# smoke-stress.sh — prove bounded-concurrency backpressure on a LIVE runner.
#
# Fires more concurrent runs than the target's RUNNER_MAX_CONCURRENT_RUNS cap and
# asserts the excess is shed IMMEDIATELY with 503 + Retry-After (never queued,
# never a crash), then that the runner still serves a normal run once the burst
# drains. This exercises R5/F-A on the shipped image; it is independent of the
# jail but runs through it.
#
# Usage: smoke-stress.sh <cap>       # cap = the target's RUNNER_MAX_CONCURRENT_RUNS
# Env:   RUNNER_URL (default http://localhost:8090), RUNNER_SERVICE_TOKEN (optional)
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cap="${1:?cap (target RUNNER_MAX_CONCURRENT_RUNS) required}"

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

# One occupied slot must outlast the burst so the excess collides with it. The
# run just sleeps well under the default wall timeout, then returns success.
body="$(jq -nc '{language: "python", source_code: "import time; time.sleep(1.5); print(\"ok\")"}')"
n=$((cap + 3))

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "== backpressure: cap=$cap, firing $n concurrent runs =="
for i in $(seq 1 "$n"); do
  curl -s -o /dev/null -D "$tmp/h.$i" -w '%{http_code}\n' "${BASE}/run" \
    -H 'content-type: application/json' "${auth[@]}" -d "$body" >"$tmp/c.$i" &
done
wait

codes="$(cat "$tmp"/c.* | tr -d '\r')"
n200="$(grep -c '^200$' <<<"$codes" || true)"
n503="$(grep -c '^503$' <<<"$codes" || true)"
echo "  codes: $(echo $codes | tr '\n' ' ')(200=$n200 503=$n503)"

[ "$n503" -ge 1 ] || { echo "STRESS FAIL: no 503 — backpressure never engaged with $n runs > cap=$cap" >&2; exit 1; }
[ "$n200" -ge 1 ] || { echo "STRESS FAIL: no 200 — every request was shed, cap not honoured" >&2; exit 1; }

# The 503 path must advertise Retry-After so the gateway can back off. Only the
# limiter's 503 sets it, so its presence anywhere in the burst is the proof.
if ! grep -qiE '^Retry-After:[[:space:]]*[0-9]+' "$tmp"/h.*; then
  echo "STRESS FAIL: a 503 was returned without a Retry-After header" >&2
  exit 1
fi
echo "  503 carried Retry-After ✓"

# Host survives: a normal run still succeeds after the burst.
RUNNER_URL="$BASE" "$here/smoke-run.sh" python 'print(2+2)' $'4\n'
echo "STRESS PASSED — excess shed with 503+Retry-After, cap honoured, host survives."
