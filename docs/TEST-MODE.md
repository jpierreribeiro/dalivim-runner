# Test-runner mode (`mode:"test"`) — G9

Test mode lets the backend grade *"implement function `X`; we run our hidden
tests against it"* instead of only *"read stdin, print stdout"*. The runner
launches the language's **own** test framework over the submission and returns the
framework's machine-readable report **raw**, plus a `success` / `tests_failed`
status.

**The runner stays a dumb, contained executor.** It writes no test, compares
nothing, decides no grade. The framework judges its own assertions; the runner
only *transcribes* the report it produced. Turning `3/5 passed` into AC/WA is the
**backend's** job — read `test_report` and decide.

> **Status:** ships **Python (pytest)** and **Go (`go test`)**. `javascript`
> (`node --test`) and `java` (JUnit) follow behind the same flag. Ask for a
> language via `GET /languages` (`test:true`).

---

## Quick start

A submission is the set `{student files + hidden test files}`, assembled by the
backend, sent in one request. The student never sees the request, so "hidden" is
purely a backend concern — no new secrecy mechanism.

```bash
curl -fsS "$RUNNER_URL/run" \
  -H 'content-type: application/json' \
  -H "X-Runner-Token: $RUNNER_SERVICE_TOKEN" \
  -d '{
    "language": "python",
    "mode": "test",
    "files": [
      {"path": "solution.py",      "content": "def add(a, b):\n    return a + b\n"},
      {"path": "test_solution.py", "content": "from solution import add\ndef test_a():\n    assert add(2, 3) == 5\ndef test_b():\n    assert add(0, 0) == 0\n"}
    ]
  }'
```

Response when every test passes:

```json
{
  "status": "success",
  "exit_code": 0,
  "report_format": "junit-xml",
  "test_report": "<?xml version=\"1.0\" encoding=\"utf-8\"?><testsuites ... tests=\"2\" failures=\"0\" ...>...</testsuites>",
  "stdout": "..\n2 passed in 0.01s\n",
  "stderr": "",
  "duration_ms": 210,
  "memory_kb": 24576,
  "runtime_name": "python",
  "runtime_version": "3.12.3"
}
```

When some tests fail — note the status is **`tests_failed`**, *not*
`runtime_error`:

```json
{
  "status": "tests_failed",
  "exit_code": 1,
  "report_format": "junit-xml",
  "test_report": "<?xml ... tests=\"4\" failures=\"2\" ...>...<testcase .../><failure ...>...</testcase>...</testsuites>",
  "stdout": "..FF\n2 failed, 2 passed in 0.02s\n"
}
```

The backend parses `test_report` (JUnit XML here) to see *which* tests failed and
compute the grade.

---

## Request fields

| Field | Meaning |
|---|---|
| `language` | as usual (`python` today) |
| `mode` | `"test"`. Absent / `"run"` is the unchanged program execution. |
| `files[]` | the `{student + hidden test}` tree, same shape as G3 multi-file. Or use `source_code` for a single self-contained test file. |
| `timeout_ms` | optional; clamped to the **test** envelope (larger than the run envelope — a suite runs longer than one program), `RUNNER_TEST_TIMEOUT_MS` … `RUNNER_MAX_TEST_TIMEOUT_MS`. |
| `memory_mb` | optional; same run-mode budget. |

**Ignored in test mode:** `stdin` (a suite defines its own inputs). A `stdins[]`
**batch with `mode:"test"` is rejected (400)** — test mode returns one report, not
per-input results.

`entrypoint` is not used: the framework **discovers** its own tests over the tree.

## Response fields (added by test mode)

| Field | Meaning |
|---|---|
| `status` | `success` (all passed), `tests_failed` (ran, ≥1 failed), or a crash/limit status (below). |
| `test_report` | the framework's machine-readable report, **verbatim**. Opaque to the runner. |
| `report_format` | how to parse it: `"junit-xml"` (pytest), `"go-test-json"` (go test). |
| `test_report_truncated` | `true` if the report hit `RUNNER_MAX_TEST_REPORT_BYTES` and was cut — read this before trusting the report is complete. |
| `exit_code` | the framework's exit code (pytest: `0` pass, `1` some failed). |

`stdout` / `stderr` still carry the framework's console output (useful for
debugging), but the **report** is the thing to grade from.

---

## Status semantics — crash vs. failure

The single most important distinction: a **failing test** is not a **crashing
harness**.

| Outcome | `status` | How it's decided |
|---|---|---|
| all tests passed | `success` | framework exit `0` |
| some tests failed | `tests_failed` | framework exit `1` **and** a report was produced |
| the submission didn't compile (Go: build/`vet` error) | `compile_error` | go test's `build-fail` marker in the report |
| harness crashed before completing (import/collection error, syntax error, no tests, segfault) | `runtime_error` | any other exit; a report may still exist but the exit code is not the tests-failed code |
| exceeded the wall/CPU budget | `timeout` | deadline hit |
| exceeded memory | `memory_exceeded` | cgroup OOM |
| flooded output past the cap | `output_limit_exceeded` | output cap hit |

The runner tells "tests failed" from "harness crashed" by the **presence of a
completed report + the framework's tests-failed exit code**, never by guessing
from stderr. So: a hidden test that fails to `import` the student's module is
`runtime_error` (the suite never ran), distinct from a student whose code made a
test assertion fail (`tests_failed`).

**Grade only `tests_failed` and `success` from the report.** Treat
`runtime_error` / `timeout` / `memory_exceeded` / `output_limit_exceeded` as
"the run didn't produce a gradeable result" (bad submission or resource abuse),
and a `503` / `internal_error` as *our* infrastructure failure to retry.

---

## Per-language behaviour

### Python — pytest

- Command (fixed, in-jail): `python3 -s -P -m pytest -p no:cacheprovider
  --tb=short -q --junit-xml=report.xml <target>`.
- Report: **JUnit XML** (`report_format:"junit-xml"`).
- Determinism: test order is collection order (no shuffling plugin), the cache is
  off, and `PYTHONHASHSEED=0` is pinned (so a test that prints a `set`/`dict` is
  stable). `PYTEST_DISABLE_PLUGIN_AUTOLOAD=1` keeps third-party plugins off the
  tree — no network/plugin surface.
- Fixtures: the test file policy admits **`conftest.py`** (pytest's fixture/hook
  file). Build/dependency manifests (`setup.py`, `pyproject.toml`) stay
  **forbidden** — a test run never processes a manifest.
- Imports: a hidden test's `from solution import ...` resolves to the sibling
  student file (pytest inserts the test directory on `sys.path`); the workdir root
  stays off `sys.path`, so a student cannot plant a module to hijack an import.

### Go — `go test`

- Command (fixed, in-jail): `go test -json -p 1 -count=1 ./...`, run in a **single
  jail** that has the Go toolchain (unlike run mode's split compile/run jails — `go
  test` compiles *and* runs in one invocation). The pre-warmed build cache is seeded
  so a run is fast, not a cold stdlib rebuild.
- Report: **go-test-json** (`report_format:"go-test-json"`) — one JSON event per
  line, on stdout.
- Layout: the runner **synthesizes** the module (`go.mod`); a caller-supplied
  `go.mod` is **forbidden**. Put the student code and the hidden `*_test.go` in the
  same package. A single `source_code` submission is written to `main_test.go`.
- Offline & deterministic: `GOPROXY=off` + `-mod=readonly` (an external import fails
  closed as a build error), `-p 1 -count=1` (no package parallelism, no result
  cache), `GOMAXPROCS=1`.
- **Build failure vs. test failure:** `go test -json` reports a compile error as an
  `"Action":"build-fail"` event and exits `1` — the *same* exit code as a test
  failure. The runner distinguishes them by that structured marker and classifies a
  build failure as **`compile_error`** (details in the report), not `tests_failed`.
- Note: `go test` runs `go vet` by default, so a `vet` error is a build failure
  (`compile_error`), not a test that ran.

---

## Containment

Test mode runs the **same untrusted student code** as run mode and gets the **same
jail**: empty network namespace (no egress), read-only host rootfs, per-run cgroup
+ rlimits, seccomp **denylist** (test frameworks fork workers and touch a wide
syscall surface, so never the static allowlist), and framework parallelism
disabled so worker forks stay inside the per-run process cap.

Report hand-back differs by framework, and neither weakens the jail:

- **pytest** writes a report *file*, so its jail binds the per-run `/sandbox`
  **writable** (the jail's `/tmp` is a private tmpfs the host can't read — same
  hand-back the compile phase uses for its artifact). This widens **only** the
  ephemeral per-run workdir.
- **go test** reports on *stdout*, so its jail keeps `/sandbox` **read-only** — the
  toolchain writes only to the size-capped `/tmp` tmpfs.

In both cases **the host rootfs stays read-only** and the network stays empty. The
on-target smoke (`scripts/smoke-test.sh`, the G9 block in `ci.yml`) proves a hidden
test cannot write outside its workdir and cannot reach the network.

Report parsing stays in the **backend**: the runner passes the report through raw
and grows no framework-specific parser, so a malformed report is the backend's
problem to reject, not a new attack surface in the runner.

---

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `RUNNER_TEST_TIMEOUT_MS` | `15000` | default wall/CPU budget for a `mode:"test"` run (a suite runs longer than one program). |
| `RUNNER_MAX_TEST_TIMEOUT_MS` | `30000` | hard ceiling a request may not exceed. |
| `RUNNER_MAX_TEST_REPORT_BYTES` | `4000000` | cap on the returned `test_report`; a larger report is cut and `test_report_truncated` set. |

Memory reuses the run-mode budget (`RUNNER_DEFAULT_MEMORY_MB` /
`RUNNER_MAX_MEMORY_MB`). The image ships `pytest` pinned (see the `Dockerfile`).

---

## Discovery

`GET /languages` advertises which languages accept test mode:

```json
{ "id": "python", "kind": "interpreted", "multifile": true, "batch": true, "test": true }
```

Send `mode:"test"` only to a language whose `test` is `true`; anything else is a
`400`.
