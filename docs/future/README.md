# Future work — dalivim-runner roadmap

The security/hardening roadmap (F-A … F-E/R6, F-D) is **done and live in prod**.
This directory plans what makes the runner *complete and correct* as an untrusted
student-code **executor** — not a judge.

## The scope boundary (read this first)

The runner's job is narrow on purpose: **given `{language, source_code|files,
stdin, limits}`, compile/run the code in a sandbox and return `{status, stdout,
stderr, telemetry}`.** It does **not** compare output to expected, assign
verdicts (AC/WA), manage test cases, or store submissions — that is the
**backend's** job. Every item below respects that line. "Complete and perfect"
means the perfect *executor*, never absorbing the judge.

## Roadmap at a glance

| Phase | Theme | Value | Size | Breaks contract? |
|---|---|---|---|---|
| **[G1](G1-executor-robustness.md)** | Executor correctness & robustness | 🔴 high | small | no (adds one status) |
| **[G2](G2-language-coverage.md)** | Language coverage — Go, Java | 🟡 high | medium | no (additive) |
| **[G3](G3-multi-file-submissions.md)** | Multi-file submissions | 🟠 high | large | **yes** (adds `files[]`) |
| **[G4](G4-observability.md)** | Observability & operability | 🟢 medium | medium | no |
| **[G5](G5-deferred.md)** | Deferred / demand-gated | ⚪ low | — | — |
| **[G6](G6-per-language-limits.md)** | Per-language limit floors (promoted from G5) | 🟢 medium | small | no |
| **[Adding a language](ADDING-A-LANGUAGE.md)** | Extension guide (the 3 runtime shapes) | — | — | — |

Recommended order is the value order above: **G1 → G2 → G3 → G4**. G1 is small and
removes real student-facing bugs; do it first. G3 is the biggest and touches the
backend; schedule it deliberately.

## How each spec is written

Every `G*.md` follows the same shape so an agent (or you) can pick one up and
implement without re-discovering the code:

1. **Motivation** — the concrete problem, in student/operator terms.
2. **Current state** — what exists today, with `file:line` anchors.
3. **Proposed change** — the design, with the exact edit locus.
4. **Contract / config impact** — new fields, env vars, statuses.
5. **Security considerations** — what the change must not weaken.
6. **Testing** — unit + on-target acceptance.
7. **Effort** — rough size and dependencies.

## Two design decisions already locked (2026-07-09)

- **Output flood → kill immediately.** When a run exceeds the output cap, end it
  with a dedicated `output_limit_exceeded` status instead of truncating and
  letting it burn CPU to the timeout. (G1.)
- **Security posture default stays `auto`.** `require` is *recommended* for prod
  (documented in DEPLOY.md), but the binary default remains `auto` so CI / local
  / a managed PaaS can run without the sandbox. (G1 documents; no code default
  change.)

## Current capability baseline (as of F-D, on `main`)

- **Languages**: `python` (3.12, `-I`), `javascript` (node, `--disable-proto`
  `--max-old-space-size`), `c` (`gcc -O2 -static`), `cpp` (`g++ -O2 -static
  -std=c++20`). Closed registry by design.
- **Sandbox**: nsjail (ro/minimal rootfs, user/mount/pid/ipc/net ns, no_new_privs,
  per-jail rlimits, seccomp denylist; **static allowlist** for the C/C++ run jail),
  empty netns, per-run cgroup v2 (`memory.max`/`pids.max`, authoritative OOM).
- **Limits**: wall timeout **and** CPU-time (`--rlimit_cpu`), memory (RLIMIT_AS +
  cgroup), pids/nproc, file size, per-stream 64 KiB output cap, source size,
  separate compile budget.
- **Status set**: `success`, `runtime_error`, `timeout`, `memory_exceeded`,
  `compile_error`, `internal_error`.
- **Ops**: `GET /healthz` only; concurrency-bounded (8, 503 on overload); slog
  JSON boot logs; no metrics, no per-run logs, no request IDs.

The gaps this roadmap closes are catalogued per phase.
