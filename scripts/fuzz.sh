#!/usr/bin/env bash
#
# fuzz.sh — run every native-Go fuzz target (S1) in the given packages for a
# bounded budget, one target at a time.
#
# Why the loop: `go test -fuzz` fuzzes exactly ONE target per invocation — a
# regex that matches more than one target is a hard error ("will not fuzz, -fuzz
# matches more than one fuzz test"). So we enumerate the Fuzz* functions in each
# package and drive each with its own anchored `-fuzz=^Name$`.
#
# Usage:
#   scripts/fuzz.sh [pkg ...]           # default: the two validator packages
# Env:
#   FUZZTIME   per-target budget (default 60s; CI uses a longer one weekly)
#
# Any crasher `go test` finds is written to <pkg>/testdata/fuzz/<Target>/ — commit
# it as a permanent regression seed (it is then replayed by plain `go test`) and
# fix the underlying validator fail-closed before the gate goes green again.
#
# Exit non-zero if any target crashes OR reports zero executions (a
# misconfigured -run/-fuzz filter must never silently skip a target).
set -uo pipefail

FUZZTIME="${FUZZTIME:-60s}"

pkgs=("$@")
if [ ${#pkgs[@]} -eq 0 ]; then
  pkgs=(./internal/executor ./internal/transport/httpapi)
fi

fail=0
ran=0
for pkg in "${pkgs[@]}"; do
  # `go test -list` matches test/benchmark/fuzz/example names; keep only Fuzz*.
  targets="$(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep -E '^Fuzz' || true)"
  if [ -z "$targets" ]; then
    echo "no fuzz targets in $pkg — skipping"
    continue
  fi
  for target in $targets; do
    ran=1
    echo "=== fuzzing $target in $pkg (fuzztime=$FUZZTIME) ==="
    out="$(mktemp)"
    if go test -run '^$' -fuzz "^${target}$" -fuzztime "$FUZZTIME" "$pkg" 2>&1 | tee "$out"; then
      # A completed run must show at least one execution, else the filter matched
      # nothing meaningful and the "green" is a false pass.
      if ! grep -qE 'execs: [1-9]' "$out"; then
        echo "::error::fuzz target $target in $pkg reported no executions"
        fail=1
      fi
    else
      echo "::error::fuzz target $target in $pkg found a crasher (see $pkg/testdata/fuzz)"
      fail=1
    fi
    rm -f "$out"
  done
done

if [ "$ran" -eq 0 ]; then
  echo "::error::no fuzz targets were run"
  exit 1
fi
exit "$fail"
