#!/usr/bin/env bash
#
# smoke-test.sh — POST a mode=test (G9) /run against a live, nsjail-backed runner
# and assert the outcome. The test-runner counterpart to smoke-files.sh: it sends
# a files[] submission with mode:"test", so the on-target CI exercises the real
# test-framework path THROUGH the jail — most importantly the report hand-back,
# which under nsjail travels the writable /sandbox bind-mount (a code path the unit
# suite's netns backend does not exercise).
#
# Usage:
#   scripts/smoke-test.sh <language> <expected_status> [expected_report_format]
#
# The files[] array is read from SMOKE_FILES as a JSON array of {path,content}.
#
# Asserts (exit non-zero on any mismatch):
#   - HTTP call succeeds,
#   - .status          == expected_status         (default: "success"),
#   - .report_format   == expected_report_format  (default: "junit-xml"),
#   - .test_report     is NON-EMPTY               (the framework's report round-tripped).
#
# Env:
#   RUNNER_URL             base URL of the runner   (default http://localhost:8090)
#   RUNNER_SERVICE_TOKEN   sent as X-Runner-Token   (optional; dev boot needs none)
#   SMOKE_FILES            JSON array of {path,content} (REQUIRED)
#   SMOKE_TIMEOUT_MS       optional timeout_ms for the suite
set -euo pipefail

BASE="${RUNNER_URL:-http://localhost:8090}"
lang="${1:?language required}"
expected_status="${2:-success}"
expected_format="${3:-junit-xml}"
files="${SMOKE_FILES:?SMOKE_FILES is required: a JSON array of path/content objects}"

auth=()
if [ -n "${RUNNER_SERVICE_TOKEN:-}" ]; then
  auth=(-H "X-Runner-Token: ${RUNNER_SERVICE_TOKEN}")
fi

body="$(jq -nc \
  --arg lang "$lang" \
  --argjson files "$files" \
  --arg timeout "${SMOKE_TIMEOUT_MS:-}" \
  '{language: $lang, mode: "test", files: $files}
   + (if $timeout == "" then {} else {timeout_ms: ($timeout|tonumber)} end)')"

resp="$(curl -fsS "${BASE}/run" \
  -H 'content-type: application/json' \
  "${auth[@]}" \
  -d "$body")"

if echo "$resp" | jq -e \
  --arg status "$expected_status" \
  --arg format "$expected_format" \
  '.status == $status and .report_format == $format and (.test_report|length) > 0' >/dev/null; then
  echo "SMOKE OK   [$lang mode=test] status=$expected_status report_format=$expected_format report_present"
  exit 0
fi

echo "SMOKE FAIL [$lang mode=test]" >&2
echo "  expected: status=$(printf '%q' "$expected_status") report_format=$(printf '%q' "$expected_format") non-empty test_report" >&2
echo "  got:      $resp" >&2
exit 1
