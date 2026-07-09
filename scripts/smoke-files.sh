#!/usr/bin/env bash
#
# smoke-files.sh — POST a MULTI-FILE /run against a live runner and assert the
# outcome. The G3 counterpart to smoke-run.sh: where that sends one source_code
# string, this sends a files[] array (and an optional entrypoint), so the
# on-target CI can exercise real multi-file submissions through the jail.
#
# Usage:
#   scripts/smoke-files.sh <language> <expected_stdout> [expected_status] [entrypoint]
#
# The files[] array is read from the SMOKE_FILES env var as a JSON array of
# {path, content} objects, e.g.
#
#   SMOKE_FILES='[{"path":"main.c","content":"..."},{"path":"util.c","content":"..."}]' \
#     scripts/smoke-files.sh c $'5\n'
#
# Asserts (exit non-zero on any mismatch):
#   - HTTP call succeeds,
#   - .status  == expected_status  (default: "success"),
#   - .stdout  == expected_stdout  (EXACT match, newlines included).
#
# Env:
#   RUNNER_URL             base URL of the runner   (default http://localhost:8090)
#   RUNNER_SERVICE_TOKEN   sent as X-Runner-Token   (optional; dev boot needs none)
#   SMOKE_FILES            JSON array of {path,content} (REQUIRED)
#   SMOKE_STDIN            optional stdin fed to the submission (default: none)
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
lang="${1:?language required}"
expected_stdout="${2?expected_stdout required}"
expected_status="${3:-success}"
entrypoint="${4:-}"
files="${SMOKE_FILES:?SMOKE_FILES is required: a JSON array of path/content objects}"

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

# Assemble {language, files, [entrypoint], [stdin]} from the pieces. jq validates
# that SMOKE_FILES is well-formed JSON before we send it.
body="$(jq -nc \
  --arg lang "$lang" \
  --argjson files "$files" \
  --arg entry "$entrypoint" \
  --arg stdin "${SMOKE_STDIN:-}" \
  '{language: $lang, files: $files}
   + (if $entry == "" then {} else {entrypoint: $entry} end)
   + (if $stdin == "" then {} else {stdin: $stdin} end)')"

resp="$(curl -fsS "${BASE}/run" \
  -H 'content-type: application/json' \
  "${auth[@]}" \
  -d "$body")"

if echo "$resp" | jq -e \
  --arg status "$expected_status" \
  --arg stdout "$expected_stdout" \
  '.status == $status and .stdout == $stdout' >/dev/null; then
  echo "SMOKE OK   [$lang multi-file] status=$expected_status stdout matched"
  exit 0
fi

echo "SMOKE FAIL [$lang multi-file]" >&2
echo "  expected: status=$(printf '%q' "$expected_status") stdout=$(printf '%q' "$expected_stdout")" >&2
echo "  got:      $resp" >&2
exit 1
