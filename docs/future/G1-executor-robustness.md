# G1 — Executor correctness & robustness

Small, high-value fixes that remove real student-facing bugs and abuse vectors in
what the runner *already does*. No new languages, no contract-breaking changes
(one additive status). **Do this phase first.**

Items: G1.1 link flags (`-lm`), G1.2 wire `compile_timeout_ms`, G1.3 stdin cap,
G1.4 kill-on-output-flood, G1.5 retire deprecated surface.

---

## G1.1 — Per-language link flags (fixes C `<math.h>`)

### Motivation
A student submitting ordinary C that uses `<math.h>` (`sqrt`, `pow`, `sin`, …
anything not constant-folded) gets `compile_error` today — the linker can't
resolve the symbol because the math library isn't linked. This is a correctness
bug that looks like the student's fault. C++ is unaffected (the g++ driver folds
`-lm` in and libstdc++ is linked automatically).

### Current state
Compile argv are literal token slices with no place for link libraries:
- C: `compiled.go:36` → `["gcc","-O2","-static","-o","{out}","{src}"]`
- C++: `compiled.go:44` → `["g++","-O2","-static","-std=c++20","-o","{out}","{src}"]`
- `compiledLangSpec` (`compiled.go:23-29`) carries `compile` and `run` slices only.

The bug is also a *link-order* trap: gcc resolves libraries left-to-right and
`{src}` is last with nothing after it, so even adding `-lm` before `{src}` would
be fragile.

### Proposed change
Add a `link []string` field to `compiledLangSpec` and render it **after** the
source/object on the link line, so symbol resolution is correct regardless of
which libs are added later:

```go
type compiledLangSpec struct {
    name    string
    binNames []string
    compile []string   // ... -o {out} {src}
    link    []string   // appended AFTER {src}: e.g. []string{"-lm"}
    run     []string
}
// c:   compile: ["gcc","-O2","-static","-o","{out}","{src}"], link: ["-lm"]
// cpp: link: nil  (driver handles it)
```

Render order in the compile-jail argv builder: `compile... , link...`. Keep
`-static` — the static-binary invariant is what lets the run jail use the tight
seccomp allowlist (see [ADDING-A-LANGUAGE](ADDING-A-LANGUAGE.md)).

> Alternative considered: `-Wl,--start-group … --end-group`. Overkill for a
> single lib; the `link`-after-`{src}` field is simpler and extensible.

### Contract / config impact
None. Programs that already compiled still compile; ones that needed `-lm` now do.

### Security considerations
`-lm` is libm, no new attack surface. Do **not** add `-l` flags that pull in
networking/dynamic-loading libs. Keep `-static` (dynamic linking would break the
minimal-rootfs run jail and the allowlist, which omits `openat` on shared libs).

### Testing
- Unit: a C spec with `link:["-lm"]` renders `… {src} -lm`.
- On-target: `#include <math.h>` … `sqrt(i)` C program → `success` (this is the
  exact program that returned `compile_error` during the F-D bring-up).
- Regression: existing hello-world C/C++ still `success` under `enforce` seccomp.

### Effort
XS — one field + one program to prove it. New image.

---

## G1.2 — Honour `compile_timeout_ms` from the request

### Motivation
The wire contract advertises a per-request compile timeout, but it's silently
ignored — a caller that sets it sees no effect. Either wrong docs or a dropped
feature; a judge that wants a tighter compile bound for a specific task can't get
one.

### Current state
`RunRequest.CompileTimeoutMs` exists (`contract.go:22-25`) but the compiled
runtime clamps the *service default* `CompileTimeoutMs` (`compiled.go:138`) and
never reads `req.CompileTimeoutMs`.

### Proposed change
Mirror the run-timeout clamping logic (`executor.go:76`): when
`req.CompileTimeoutMs > 0`, clamp it to `[1, MaxCompileTimeoutMs]` and use it;
else fall back to `CompileTimeoutMs`. Do the clamp in the service layer next to
the existing timeout/memory clamping, not in the runtime.

### Contract / config impact
`compile_timeout_ms` becomes functional (was a no-op). No new field.

### Security considerations
Clamp to `MaxCompileTimeoutMs` — never let a request *raise* the compile ceiling,
only lower it within bounds (a long compile is a DoS vector; the ceiling exists
for that reason).

### Testing
Unit: request compile timeout below/above the ceiling clamps correctly; zero
falls back to default. On-target: a template-heavy C++ program with a tiny
`compile_timeout_ms` → `compile_error` with `[compile timed out]`.

### Effort
XS.

---

## G1.3 — Dedicated stdin size cap

### Motivation
Judge inputs can be large (a program reading 10⁵ integers). Today stdin has no
cap of its own — it's bounded only by the shared JSON-body limit (source + 64 KiB
slack), so a big input eats into the source budget and vice-versa. The two should
be independent and independently tunable.

### Current state
`MaxBytesReader` bounds the whole body at `MaxSourceBytes + 64 KiB`
(`handlers.go:48`); there is no `stdin`-specific limit. `RUNNER_MAX_SOURCE_BYTES`
exists; there is no `RUNNER_MAX_STDIN_BYTES`.

### Proposed change
Add `RUNNER_MAX_STDIN_BYTES` (default e.g. `1_000_000` — 1 MB, judge inputs are
often bigger than 64 KiB) and validate `len(req.Stdin)` against it in the handler,
returning `400` with a clear error. Raise the `MaxBytesReader` bound to
`MaxSourceBytes + MaxStdinBytes + slack` so a legitimate large stdin isn't cut off
at the body layer.

### Contract / config impact
New env `RUNNER_MAX_STDIN_BYTES`. New `400` validation path (bad request, not a
run outcome). No new run status.

### Security considerations
Cap is a memory-DoS bound; keep it finite. stdin is fed to the sandboxed child,
never interpreted by the runner, so content is not a concern — only size.

### Testing
Unit: stdin at/over the cap → 400; under → passes. On-target: a program summing N
stdin integers with N large enough to exceed 64 KiB → `success`.

### Effort
S.

---

## G1.4 — Kill on output flood (`output_limit_exceeded`)

### Motivation
A `while(1) printf("x")` (or `print` loop) is a common accidental — or
malicious — submission. Today the runner truncates each stream at 64 KiB but the
**child keeps running**, burning a full CPU-second budget until the timeout, and
the student waits the whole timeout for a verdict. A judge should fail fast: once
output is clearly unbounded, stop.

### Current state
`limitedBuffer{limit: RUNNER_MAX_OUTPUT_BYTES}` (`buffer.go`) counts and discards
bytes past 64 KiB per stream and appends `\n[output truncated]`
(`interpreted.go:115-118`, `compiled.go:225-228`), but has no side effect on the
process — the run only ends on exit/timeout. The cap is also **per-stream**
(64 KiB stdout + 64 KiB stderr), not combined.

### Proposed change (decision locked: kill immediately)
When either stream's discarded-byte counter first crosses the limit, cancel the
run's context (the same mechanism the timeout uses — `CancelCmd` SIGKILLs the
process group, `sandbox_linux.go:331-337`) and classify the result as a new
status **`output_limit_exceeded`**. Precedence: check it *before* `runtime_error`
but after `timeout`/`success`/`memory_exceeded`, so a program that floods output
and then a deadline races resolves deterministically (prefer the output verdict
since it's the earlier cause). Consider also a **combined** cap
(`stdout+stderr ≤ N`) rather than per-stream, so `stderr` flooding is covered too.

Wiring: `limitedBuffer` needs a way to signal "limit hit" (e.g. a `context.CancelFunc`
or a channel it closes on first overflow) so the run layer can cancel. Keep the
truncation + marker for the bytes already captured, so the caller still sees the
first 64 KiB.

### Contract / config impact
**New status `output_limit_exceeded`** — add to `contract.go:60-67` and document
in TESTING.md/DEPLOY. Backend should map it to a student-facing "output too large"
verdict. Optionally a new `RUNNER_MAX_OUTPUT_BYTES` stays the knob; add
`RUNNER_OUTPUT_LIMIT_COMBINED=true|false` if you want to choose per-stream vs
combined.

### Security considerations
This *strengthens* containment (bounds CPU wasted on output floods). Ensure the
cancel path reaps the whole process group (it already does for timeouts) so no
grandchild keeps writing.

### Testing
- Unit: buffer signals on first overflow; run classifies `output_limit_exceeded`.
- On-target: `python -c "while True: print('x')"` → `output_limit_exceeded` in
  well under the timeout (measure `duration_ms` ≪ timeout). C `for(;;)putchar('x')`
  → same.
- Regression: a program printing exactly < 64 KiB → still `success`, full output.

### Effort
S–M (touches buffer, all run paths, status enum, docs).

---

## G1.5 — Retire deprecated surface

### Motivation
Carry-over from the extraction: a deprecated endpoint and a duplicated response
field. Dead weight that widens the contract and the test matrix.

### Current state
- `POST /run/python` alias (`server.go:45`, `handlers.go:36-43`) — superseded by
  `POST /run` with explicit `language`.
- `python_version` field (`contract.go:50-54`) — duplicate of `runtime_version`,
  populated only for Python.

### Proposed change
Coordinate with the backend first (confirm nothing calls `/run/python` or reads
`python_version`), then remove both in one release. Until the backend is
confirmed migrated, leave them and just mark the removal ticket.

### Contract / config impact
Removal is a breaking change **iff** a caller still uses them — hence the
backend-coordination gate. After removal the contract is `POST /run` +
`runtime_version` only.

### Security considerations
None (smaller surface is marginally better).

### Testing
Grep the backend for `/run/python` and `python_version`; remove; run the Postman
suite (`postman/`) to confirm nothing 404s.

### Effort
XS — gated on backend confirmation.

---

## Phase G1 acceptance

- C with `<math.h>` → `success` (G1.1).
- `compile_timeout_ms` in a request measurably changes the compile bound (G1.2).
- Large stdin (> 64 KiB) works; oversized stdin → 400 (G1.3).
- Output flood → `output_limit_exceeded`, `duration_ms ≪ timeout` (G1.4).
- Deprecated surface removed or ticketed with backend sign-off (G1.5).
- Full `verify` (`deploy/deploy.sh verify`) + Postman suite still green.
