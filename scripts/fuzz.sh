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
# A REAL finding (a crash or a failed invariant) is written by `go test` to
# <pkg>/testdata/fuzz/<Target>/ — commit it as a permanent regression seed (it is
# then replayed by plain `go test`) and fix the underlying validator fail-closed
# before the gate goes green again. That reproducer FILE — not the exit code — is
# the authoritative "real bug" signal, because `go test -fuzz` also exits non-zero
# on a SPURIOUS shutdown timeout ("context deadline exceeded" when the coordinator
# can't stop a worker at the -fuzztime boundary under load; golang/go#48591 /
# #51484). That flake writes NO reproducer, so we distinguish it from a real crash
# and retry once instead of reddening CI on an infra hiccup.
#
# Exit non-zero if any target produces a reproducer OR reports zero executions (a
# misconfigured -run/-fuzz filter must never silently skip a target).
set -uo pipefail

FUZZTIME="${FUZZTIME:-60s}"

pkgs=("$@")
if [ ${#pkgs[@]} -eq 0 ]; then
  pkgs=(./internal/executor ./internal/transport/httpapi)
fi

# run_fuzz <pkg> <target> — run one target once. Returns:
#   0  clean pass (fuzzed, executions > 0, no reproducer)
#   1  real failure (reproducer written, or zero executions, or an unknown error)
#   2  spurious shutdown timeout (non-zero exit, NO reproducer) — caller may retry
run_fuzz() {
  local pkg="$1" target="$2" out rc crasher_dir
  crasher_dir="${pkg#./}/testdata/fuzz/${target}"
  out="$(mktemp)"
  go test -run '^$' -fuzz "^${target}$" -fuzztime "$FUZZTIME" "$pkg" 2>&1 | tee "$out"
  rc=${PIPESTATUS[0]}
  if [ "$rc" -eq 0 ]; then
    # A completed run must show at least one execution, else the filter matched
    # nothing meaningful and the "green" is a false pass.
    if grep -qE 'execs: [1-9]' "$out"; then rm -f "$out"; return 0; fi
    echo "::error::fuzz target $target in $pkg reported no executions"
    rm -f "$out"; return 1
  fi
  # Non-zero exit. A reproducer under testdata/fuzz means a REAL finding.
  if [ -d "$crasher_dir" ] && [ -n "$(ls -A "$crasher_dir" 2>/dev/null)" ]; then
    echo "::error::fuzz target $target found a CRASHER — reproducer in $crasher_dir (commit it as a seed, fix fail-closed)"
    rm -f "$out"; return 1
  fi
  # No reproducer + the known shutdown-timeout signature => spurious infra flake.
  if grep -q 'context deadline exceeded' "$out"; then
    echo "::warning::$target: spurious 'context deadline exceeded' at shutdown, no reproducer (golang/go#48591)"
    rm -f "$out"; return 2
  fi
  echo "::error::fuzz target $target in $pkg failed with no reproducer and no known-flake signature"
  rm -f "$out"; return 1
}

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
    run_fuzz "$pkg" "$target"; rc=$?
    if [ "$rc" -eq 2 ]; then
      echo "=== retrying $target once (spurious shutdown timeout, no reproducer) ==="
      run_fuzz "$pkg" "$target"; rc=$?
      # Two spurious timeouts with no reproducer either time is infra, not a bug —
      # a real finding writes a reproducer on the very run it is found. Warn, pass.
      [ "$rc" -eq 2 ] && { echo "::warning::$target: spurious timeout twice, still no reproducer — treating as pass"; rc=0; }
    fi
    [ "$rc" -eq 0 ] || fail=1
  done
done

if [ "$ran" -eq 0 ]; then
  echo "::error::no fuzz targets were run"
  exit 1
fi
exit "$fail"
