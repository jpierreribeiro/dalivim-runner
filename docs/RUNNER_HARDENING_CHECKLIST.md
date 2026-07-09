# Runner Hardening Checklist

Living tracker for the roadmap defined in
[RUNNER_AUDIT_AND_CONTRACT.md](./RUNNER_AUDIT_AND_CONTRACT.md) and
[RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md](./RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md).

Legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked

---

## Milestone status

| Phase | Goal | Status |
|---|---|---|
| Extraction | Lift monolith `runner-service/` → clean-arch repo | [x] done |
| Audit | 8-layer conformance + residual-risk ranking + API contract | [x] done |
| **F-A** | Kill global `RLIMIT_NPROC`; add concurrency cap + 503 backpressure | [x] done (verified live) |
| Refactor | Invert Sandbox: `SysProcAttr()` provider → `Command(Spec)` constructor | [x] done |
| F-B | `nsjailSandbox`: seccomp, tmpfs-capped `/tmp`, `no_new_privs`, per-jail nproc | [x] code done (target validation pending) |
| F-C / F-D | Compiled-language support (compile phase, `signal`, polyglot runtimes) | [ ] todo |
| F-E / F-F | cgroups accounting, deterministic `memory_exceeded` classification | [ ] todo |

---

## F-A — remove auto-DoS, add backpressure  *(immediate)*

Fixes **R1** (confirmed `errno=11` boot/fork failure) and **R5** (no backpressure).

- [x] Remove the process-wide `LimitProcesses()` call in `cmd/runner/main.go`
      (global `RLIMIT_NPROC` is a per-uid shared resource — wrong tool per D-3).
      Primitive kept in `sandbox` for the per-jail path; doc'd against reuse as a global cap.
- [x] Add a bounded concurrency semaphore — `httpapi.LimitConcurrency` middleware,
      placed inside `RequireToken` so only authenticated callers consume a slot.
- [x] Return `503` + `Retry-After` when saturated (fail fast, don't queue) so the Gateway can fall back.
- [x] Keep `RUNNER_MAX_PROCESSES` as the *per-run* budget for the future jail,
      not a global limit (config field re-documented as reserved).
- [x] Smoke-test boot + a real `/run` on the busy host that triggered `errno=11`:
      booted with **no** nproc override, `print(6*7)` → `42`; cap=1 → concurrent A=200 / B=503 (Retry-After: 1).
- [x] Update README security section (fork-bomb story moved to per-jail; added Overload/backpressure).

Config: `RUNNER_MAX_CONCURRENT_RUNS` (default `8`, `0` disables). Tests: `middleware_test.go`
(`503`+Retry-After under saturation, slot reuse, pass-through when unset); race-clean.

## Interface refactor — Sandbox becomes a Command factory

Precondition for F-B; must not touch the executor.

- [x] Define `Spec` (argv, workdir, timeouts, rlimits, per-run nproc, fsize).
      `internal/sandbox/sandbox.go`.
- [x] Replace `Sandbox.SysProcAttr()` with `Sandbox.Command(ctx, Spec) *exec.Cmd`
      (ctx carries the deadline; the sandbox sets the process group + cancel-kill).
- [x] Port the netns backend (`netnsSandbox`) to the new interface —
      behaviour-preserving; the F-03 egress + CPU-limit tests still pass through it.
- [x] `executor.Service` unchanged; only the Python runtime moved to `Command(Spec)`.

## F-B — nsjail sandbox

Fixes **R2** (no seccomp), **R3** (`/tmp` OOM), **R4** (no `no_new_privs`).

- [x] `nsjailSandbox` implementing `Command(ctx, Spec)` (`sandbox_linux.go`).
- [x] Args: read-only rootfs, `--tmpfsmount /tmp`, `--rlimit_nproc`,
      `--rlimit_fsize`, `--rlimit_as/_cpu`, `--time_limit`, `--seccomp_string`
      (kafel denylist), `--iface_no_lo`, `--disable_proc`; `no_new_privs` is
      nsjail's default. Pure builder in `nsjail.go`, unit-tested.
- [x] Dockerfile: nsjail built from source (bookworm stage) into the runtime image.
- [x] `RUNNER_SANDBOX=auto|require|off` dial with a boot probe (runs `/bin/true`
      in a real jail); `require` fails closed, `auto` falls back to netns. Tested.
- [~] Netns + execution suites pass against the **nsjail** backend — deferred:
      nsjail can't run in CI (no binary + no userns). Arg construction + the dial
      state machine are unit-tested; end-to-end jail execution is validated on the
      target via the boot log and a `RUNNER_SANDBOX=require` verification deploy.

## F-C / F-D — compiled languages (forward-compat, not blocking)

- [ ] Wire `compile_timeout_ms` / `compile_output` contract fields (already reserved).
- [ ] Add `signal` to `RunResult` for SIGKILL/SIGSEGV disambiguation.
- [ ] Second runtime (C/C++) behind the `language` dispatch.

## F-E / F-F — accounting & classification

Fixes **R6** (fragile `memory_exceeded` substring heuristic).

- [ ] cgroups v2 memory/CPU accounting for authoritative `memory_kb`.
- [ ] Classify `memory_exceeded` from OOM signal / cgroup event, not stderr text.

---

## Residual-risk index

| ID | Risk | Addressed by |
|---|---|---|
| R1 | Global `RLIMIT_NPROC` → auto-DoS (`errno=11`, **confirmed**) | F-A ✅ closed |
| R2 | No seccomp filter | F-B ✅ code done (kafel denylist; target-validate) |
| R3 | `/tmp` unbounded → host OOM | F-B ✅ code done (tmpfs `/tmp`; target-validate) |
| R4 | No `no_new_privs` | F-B ✅ code done (nsjail default; target-validate) |
| R5 | Runner has no backpressure (never emits 503) | F-A ✅ closed |
| R6 | `memory_exceeded` from fragile stderr substring | F-E/F-F |
