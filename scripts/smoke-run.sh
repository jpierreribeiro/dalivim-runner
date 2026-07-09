#!/usr/bin/env bash
#
# smoke-run.sh — POST /run against a live runner and assert the outcome.
#
# Usage:
#   scripts/smoke-run.sh <language> <source_code> <expected_stdout> [expected_status]
#
# Asserts (exit non-zero on any mismatch):
#   - HTTP call succeeds,
#   - .status  == expected_status  (default: "success"),
#   - .stdout  == expected_stdout  (EXACT match, newlines included).
#
# The expected_stdout is compared byte-for-byte, so pass the trailing newline
# a `print` emits, e.g.  scripts/smoke-run.sh python 'print(2+2)' $'4\n'.
#
# Env:
#   RUNNER_URL             base URL of the runner   (default http://localhost:8090)
#   RUNNER_SERVICE_TOKEN   sent as X-Runner-Token   (optional; dev boot needs none)
#   SMOKE_STDIN            optional stdin fed to the submission (default: none),
#                          so a run that reads input can be asserted end to end
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
lang="${1:?language required}"
src="${2?source_code required}"
expected_stdout="${3?expected_stdout required}"
expected_status="${4:-success}"

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

body="$(jq -nc --arg lang "$lang" --arg src "$src" --arg stdin "${SMOKE_STDIN:-}" \
  '{language: $lang, source_code: $src} + (if $stdin == "" then {} else {stdin: $stdin} end)')"

resp="$(curl -fsS "${BASE}/run" \
  -H 'content-type: application/json' \
  "${auth[@]}" \
  -d "$body")"

if echo "$resp" | jq -e \
  --arg status "$expected_status" \
  --arg stdout "$expected_stdout" \
  '.status == $status and .stdout == $stdout' >/dev/null; then
  echo "SMOKE OK   [$lang] status=$expected_status stdout matched"
  exit 0
fi

echo "SMOKE FAIL [$lang]" >&2
echo "  expected: status=$(printf '%q' "$expected_status") stdout=$(printf '%q' "$expected_stdout")" >&2
echo "  got:      $resp" >&2
exit 1
