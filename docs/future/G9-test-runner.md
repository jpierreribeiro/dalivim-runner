# G9 — Test-runner grading mode (pytest / go test / node --test / JUnit)

The single biggest **product** unlock after the language work: let the backend
grade *"implement function `X`; we run our hidden tests against it"* instead of
only *"read stdin, print stdout"*. It stays cleanly on the **executor** side of the
line — the runner launches a test framework the student's code must satisfy and
returns the framework's **own** pass/fail report **raw**. The runner never writes a
test, never decides the submission's grade, never compares to expected output.

> **Framing discipline (load-bearing).** The framework judges its own assertions;
> the runner only *transcribes* the report it produced. The runner renders **no
> verdict of its own** — it does not know which tests are "hidden", does not weight
> them, does not turn `3/5 passed` into AC/WA. That is the backend. This is the
> same line G6 draws for `stdins[]`.

---

## Motivation

Today a submission is `source → stdout`, and the backend diffs stdout against an
expected string (`docs/future/README.md:9-14`). That fits classic I/O problems but
**not** the dominant model in modern coding courses: *test-based grading*. There
the exercise is a function/class signature, and correctness is "does the student's
code pass our test suite?" — unit tests the student never sees.

Everything needed already exists in the runner's shape:

- **The hidden test file is just another file.** G3 `files[]`
  (`pkg/runnerapi/contract.go:37-40`) already materializes an attacker-controlled
  file tree with a traversal-resistant writer (`internal/executor/materialize.go`).
  The backend assembles `{student files + hidden test files}` and sends them in one
  request; the student never sees the runner's payload, so "hidden" is purely the
  backend's concern. **No new secrecy mechanism is needed.**
- **The "program" becomes the test runner.** Instead of `python3 -I main.py`, the
  jail runs `pytest` / `go test` / `node --test` / the JUnit console launcher over
  the materialized tree.
- **The output becomes a machine-readable report** each framework already emits.

So G9 is *not* a new sandbox and *not* a judge. It is: a **closed, per-language
test-command registry** (mirroring the language registry) + a **report field** on
the result. The containment, limits, and materialization are all reused.

## Current state

- One request runs exactly one program to `stdout`/`stderr`. Dispatch is
  `Service.Run` → `Runtime.Run` (`internal/executor/executor.go:123-154`,
  interface at `:83-91`). The runtime picks the argv; the **caller cannot influence
  the run command** beyond source + stdin.
- Interpreted argv is `spec.runArgs` (interpreter + fixed source filename),
  `internal/executor/languages.go:50` and `interpreted.go:145-149`. Compiled is a
  two-jail compile→run (`internal/executor/compiled.go:362-388`).
- `files[]` materializes under `/sandbox/src` via `materializeSource`
  (`internal/executor/materialize.go:113-123`); per-language `FilePolicy`
  (`internal/executor/filepolicy.go:112-159`) governs which extensions/names are
  allowed.
- `RunResult` (`pkg/runnerapi/contract.go:78-114`) carries `stdout/stderr/status/
  exit_code/…` — **no structured report field**.
- Status set is `success/runtime_error/timeout/memory_exceeded/compile_error/
  internal_error` (`contract.go:157-171`). A test run that "completed but some tests
  failed" has **no natural status** today.

**SEAM:** the run argv is the only thing that must change. Materialization,
containment, limits, and classification are reused verbatim. The runtime already
selects argv per language — G9 adds a *second* argv shape (test mode) per language,
gated by a request flag.

## Proposed change

### Contract

Add an opt-in **mode** plus a raw **report** on the result. Default is unchanged.

```go
type RunRequest struct {
    // ... existing ...
    // Mode selects the execution shape. "" / "run" (default) is the unchanged
    // program execution. "test" runs the language's test framework over files[]
    // (or source_code) and returns its machine-readable report. Mutually
    // compatible with files[] and stdins[]? No — test mode ignores stdin/stdins
    // (a test suite defines its own inputs); reject stdins[] + mode=test with 400.
    Mode string `json:"mode,omitempty"`
}

type RunResult struct {
    // ... existing ...
    // TestReport is the test framework's machine-readable report, VERBATIM, when
    // Mode=="test". It is opaque to the runner — the backend parses it per
    // framework (the framework named in ReportFormat). Empty in run mode.
    TestReport   string `json:"test_report,omitempty"`
    ReportFormat string `json:"report_format,omitempty"` // "go-test-json" | "junit-xml" | "tap13" | "pytest-json"
}
```

New terminal status for "the suite ran, some tests failed" — distinct from a crash:

```go
// StatusTestsFailed: the test framework ran to completion and reported ≥1 failing
// test. It is NOT runtime_error (the harness itself did not crash) and NOT a judge
// verdict — the backend reads TestReport to decide the grade. exit_code is the
// framework's (non-zero). Additive: a caller that doesn't special-case it sees an
// unsuccessful run whose report explains why.
StatusTestsFailed = "tests_failed"
```

### The per-language test registry (closed, like the language registry)

A `mode=test` request resolves a **fixed, in-code** command template per language —
**never** caller-supplied (a free-form command would be arbitrary execution and
break "dumb + safe"). One closed map, edited deliberately, mirroring `filePolicies`
(`filepolicy.go:112`):

| Language | Framework | Command (in-jail) | Native report | Shape note |
|---|---|---|---|---|
| `python` | pytest | `pytest -p no:randomly -p no:cacheprovider --tb=short -q --junit-xml=/tmp/r.xml` | JUnit XML | interpreted jail |
| `javascript` | **`node --test`** (built-in) | `node --test --test-reporter=tap` | TAP13 | interpreted jail; **no node_modules** |
| `go` | `go test` | `go test -json -p 1 -count=1 ./...` | go-test-json | **single compile+run jail** (see below) |
| `java` | JUnit Platform | `java -jar junit-console.jar --scan-classpath --reports-dir=/tmp` after `javac` | JUnit XML | VM-compiled, two-phase |

Recommendations baked into the table:

- **JS: prefer `node --test` (Node 18+, built in) over Jest.** It emits TAP, needs
  **zero `node_modules`**, and keeps the image supply-chain surface flat — a real
  win for a security-focused image. Jest is supportable but requires bundling
  `node_modules` into the image (weight + CVE surface); make it a deliberate later
  choice, not the default. (The image already ships Node 18, `Dockerfile:65`.)
- **Python: pytest**, with `-p no:randomly` and `-p no:cacheprovider` pinned for
  determinism (no test-order shuffling, no `.pytest_cache` write). Ships via the
  image (`pip install pytest==<pinned>` in the runtime stage).
- **Java: the JUnit Platform Console Launcher jar**, bundled in the image and on the
  classpath; `--scan-classpath` discovers `@Test` methods after `javac`.

### Execution flow per shape

- **Interpreted (python, js):** identical to a normal interpreted run — materialize
  `files[]` under `/sandbox/src` (`materialize.go:113`), then launch the test argv
  instead of `spec.runArgs`. Read the report file from the writable `/tmp` after the
  run (the jail's `/tmp` is a per-run tmpfs; the framework writes `/tmp/r.xml`, the
  host reads it back — same pattern the compile phase uses to hand an artifact
  across jails, `compiled.go:546-552`). TAP/`node --test` writes to stdout, so no
  file hand-back is needed there.
- **Go:** `go test` **compiles and runs in one invocation**, so test mode does NOT
  use the split compile-jail/run-jail model. It runs in a **single full-rootfs,
  writable-`/tmp`, toolchain-present jail** (the *compile* jail posture,
  `compiled.go:505-516`, but kept alive to run) — because the run needs the Go
  toolchain and a writable build cache, unlike the minimal-rootfs static run jail.
  Seed `GOCACHE` from `/opt/gocache` exactly as the compile prelude does
  (`compiled.go:164`). Report is `go test -json` on stdout.
- **Java:** two-phase as today — `javac` the sources **and** the test classes into
  `/sandbox/classes`, then run the JUnit console launcher on that classpath
  (full-rootfs denylist jail, `compiled.go:559-608`). Report XML read back from
  `/tmp`.

### Classification

Reuse `execute()`'s existing switch (`compiled.go:633-647`, `interpreted.go:201-215`)
with one addition, evaluated **after** timeout/memory/output but **before** the
generic `runtime_error` default:

- framework exit code is the "some tests failed" code (pytest `1`, `go test` `1`,
  `node --test` `1`, JUnit `1`) **and** a parseable report was produced →
  `tests_failed`.
- framework crashed (segfault, OOM, import error before any test) → the existing
  `runtime_error`/`memory_exceeded`/`timeout` paths already cover it.
- all tests passed → `success`.

The runner distinguishes *"tests failed"* from *"harness crashed"* by **the presence
of a valid report**, not by guessing from stderr — the same authoritative-signal
discipline as the truncation flags.

## Contract / config impact

- Additive `mode` request field (default `""`/`run` unchanged) and `test_report`/
  `report_format` result fields; one new status `tests_failed`. Run-mode responses
  are **byte-for-byte unchanged**.
- New env:
  - `RUNNER_TEST_TIMEOUT_MS` — a suite runs longer than one program; a separate
    (larger) default ceiling than `RUNNER_MAX_TIMEOUT_MS`, clamped in the service
    like every other limit (`executor.go:193-205`).
  - `RUNNER_MAX_TEST_REPORT_BYTES` — cap the report size (a suite with thousands of
    failures) independent of the stdout cap.
- Image additions: `pytest` (pinned), the JUnit console-launcher jar (pinned). Node
  and Go bring their test runners for free.
- **No judge semantics.** The report is raw; the pass/fail → grade decision stays in
  the backend.

## Security considerations

*(Detailed threat items live in the security specs; the containment-critical points
G9 must not weaken:)*

- **The test framework runs untrusted student code** — it is the same untrusted
  execution as run mode and gets the **same jail** (ro/appropriate rootfs, empty
  netns, cgroup, rlimits, denylist). Test frameworks spawn workers and touch a wide
  syscall surface → **denylist only**, never the static allowlist. Disable framework
  parallelism (`-p 1`, `--runInBand`, single worker) so worker forks stay within the
  per-run `pids.max` and accounting stays clean.
- **Offline, no plugin/dependency fetch.** pytest must not autoload network plugins;
  Jest (if ever added) must not resolve packages off the tree; `go test` must keep
  the G3 offline module policy (`GOPROXY=off`, `internal/executor/compiled_multifile.go:52`).
  The empty netns enforces this structurally; the flags make it explicit.
- **`files[]` policy must admit test files** — the `FilePolicy` extension/name
  allowlists (`filepolicy.go:112-159`) currently forbid e.g. `conftest.py`
  (`filepolicy.go:116`). Test mode needs a **separate, still-closed** policy that
  admits the framework's fixtures (`conftest.py`, `*_test.go`, `*.test.js`) while
  keeping the manifest/dependency bans. Do **not** relax the run-mode policy.
- **Report parsing stays in the backend** (raw passthrough) → the runner grows **no
  framework-specific parser**, so a malformed report is the backend's problem to
  reject, not a new attack surface in the runner. *(If normalized JSON is chosen
  instead — see Alternatives — the parsers become runner surface and must be
  fuzzed; that is the explicit cost of that choice.)*

## Alternatives considered

- **Normalized JSON report (runner parses each framework into one schema).** Nicer
  for the backend (one parser), but it grows framework-specific parsing **inside**
  the runner and inches toward interpreting results. Given "keep the runner dumb" is
  an absolute, the **raw-passthrough** design above is the recommendation; normalized
  JSON is a deliberate future option with a real cost (parser surface → must be
  fuzzed per S1). Decide explicitly; do not drift into it.
- **Free-form run command from the caller.** Rejected: arbitrary command selection
  is arbitrary execution and breaks "dumb + safe". The closed per-language registry
  is the safe analogue of the language registry.
- **Jest as the JS default.** Rejected as default for the `node_modules` supply-chain
  weight; `node --test` gives the same capability zero-dep.

## Testing

- **Regression:** `mode` absent → response byte-for-byte the current `RunResult`.
- **Pass:** student code that satisfies the suite → `success`, report shows all
  passed, `exit_code==0`.
- **Fail:** student code that fails 2 of 5 → `tests_failed`, report enumerates the 2,
  `exit_code!=0`, **not** `runtime_error`.
- **Harness crash vs test fail:** a student file that raises at import → the run is
  `runtime_error` (no valid report), distinct from `tests_failed`.
- **Determinism:** the same suite over the same code yields byte-identical reports
  across repeated runs (order pinned, cache disabled).
- **Isolation / offline:** a test that attempts egress or a dependency fetch fails
  closed (empty netns); on-target smoke per language, mirroring `smoke-escape.sh`.
- **Go single-jail:** `go test` compiles+runs in one jail with a warm `GOCACHE`
  (wall well under `RUNNER_TEST_TIMEOUT_MS`), egress contained.
- **Caps:** a suite emitting a huge report is truncated at `RUNNER_MAX_TEST_REPORT_BYTES`
  with the truncation flag set.

## Effort

**L.** The contract and classification are small; the real work is (a) four
per-language test-command integrations, each with image additions and a report
hand-back, and (b) the Go single-jail test posture (a genuinely new run shape). Do
it **one language at a time** behind the same `mode=test` flag — python first
(pytest is the reference), then go, then js (`node --test`), then java.

## Phase G9 acceptance

- A `mode=test` request runs the language's framework over `files[]`, returns its
  **raw** report and `tests_failed`/`success`, and renders no verdict.
- Run-mode (`mode` absent) is byte-for-byte unchanged.
- Every test-mode language re-proves containment (egress, host-write, offline) in an
  on-target smoke, exactly like a new language (`ADDING-A-LANGUAGE.md:147-161`).
- Report size and suite wall-time are independently bounded; the host survives a
  pathological suite.
