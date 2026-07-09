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

**Wave 1 — the original roadmap.** G1, G2, G3, and G4 are **merged to `main`**.

| Phase | Theme | Value | Size | Breaks contract? | Status |
|---|---|---|---|---|---|
| **[G1](G1-executor-robustness.md)** | Executor correctness & robustness | 🔴 high | small | no (adds one status) | ✅ merged |
| **[G2](G2-language-coverage.md)** | Language coverage — Go, Java | 🟡 high | medium | no (additive) | ✅ merged |
| **[G3](G3-multi-file-submissions.md)** | Multi-file submissions | 🟠 high | large | **yes** (adds `files[]`) | ✅ merged |
| **[G4](G4-observability.md)** | Observability & operability | 🟢 medium | medium | no | ✅ merged |
| **[G5](G5-deferred.md)** | Deferred / demand-gated | ⚪ low | — | — | — |

**Wave 2 — correctness & operability at scale.** What makes the runner a *great*
executor for a grading platform, not just a correct one. Each item was
code-grounded before planning — two of the four candidate directions turned out to
be **already done** (graceful drain, escape corpus), so G8 captures only the real
residuals.

| Phase | Theme | Value | Size | Breaks contract? | Status |
|---|---|---|---|---|---|
| **[G6](G6-batch-execution.md)** | Batch execution (compile-once, run N stdins) | ⚪ gated¹ | medium | no (additive `stdins[]`) | ✅ implemented² |
| **[G7](G7-execution-determinism.md)** | Execution determinism (locale / TZ / env) | 🟢 hygiene¹ | **XS/S** | no | — |
| **[G8](G8-ops-containment-maintenance.md)** | Ops & containment maintenance | 🟢 medium | small | no | — |

| Guide | | | | |
|---|---|---|---|---|
| **[Adding a language](ADDING-A-LANGUAGE.md)** | Extension guide (the 3 runtime shapes) | — | — | — |

¹ **Grounded correction (2026-07-09).** A backend sweep found the platform has
**no judge today** — no expected-output/test-case model, no output comparison, one
run per submission. That **demand-gates G6** (batch `stdins[]` has no caller until a
test-case model exists) and **reframes G7** from "fixes silent WA" to cheap hygiene
that becomes a prerequisite once judging lands.

² **G6 landed 2026-07-09** by owner decision, ahead of the demand gate: the
contract is additive (`stdins[]` + batch envelope), so shipping the executor side
early costs nothing and it's ready the moment a backend test-case model exists.
See the implementation notes in [G6](G6-batch-execution.md).

Recommended order for the remaining Wave 2: **G8 → G7**. G8's residuals (esp.
token rotation and the language-parametrized escape corpus) are real now; G7 is
cheap hygiene to bank ahead of any judge. The higher-leverage
near-term work is now at the **system** level (seam consolidation, runner migration,
abuse/quota hardening) — tracked outside this runner-only roadmap.

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
