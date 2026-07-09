# Runner Hardening Checklist

Living tracker for the roadmap defined in
[RUNNER_AUDIT_AND_CONTRACT.md](./RUNNER_AUDIT_AND_CONTRACT.md) and
[RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md](./RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md).

> This tracker is the **task queue** for the spec-driven agent loop in
> [AGENT_HARDENING_LOOP.md](./AGENT_HARDENING_LOOP.md): each loop reads the plan
> (the spec), takes the next actionable item here, ships a draft PR, and updates
> these boxes.

Legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked

---

## Milestone status

| Phase | Goal | Status |
|---|---|---|
| Extraction | Lift monolith `runner-service/` → clean-arch repo | [x] done |
| Audit | 8-layer conformance + residual-risk ranking + API contract | [x] done |
| **F-A** | Kill global `RLIMIT_NPROC`; add concurrency cap + 503 backpressure | [x] done (verified live) |
| Refactor | Invert Sandbox: `SysProcAttr()` provider → `Command(Spec)` constructor | [x] done |
| F-B | `nsjailSandbox`: seccomp, tmpfs-capped `/tmp`, `no_new_privs`, per-jail nproc | [x] done (validated in CI: image builds nsjail, `RUNNER_SANDBOX=require` boots `nsjail ENABLED`, escape corpus contained) |
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
- [x] End-to-end jail execution **validated GREEN in CI** by the `runner-smoke`
      job (`.github/workflows/ci.yml`): it builds the shipped image (nsjail
      compiled from source), boots it under target-like limits with
      `RUNNER_SANDBOX=require` (fail-closed — a failed probe crashes the
      container, never a silent netns downgrade), asserts the boot log shows
      `nsjail ENABLED`, runs a real `POST /run` (`print(2+2)` → `success`/`"4\n"`),
      and the §4.4 escape corpus (`scripts/smoke-escape.sh`: fork bomb → contained
      + host survives; `/etc/shadow`/host-env read → denied; dangerous syscall →
      killed by seccomp; output flood → truncated, no OOM). Arg construction + the
      dial state machine remain unit-tested for the paths CI's single kernel
      cannot exercise.
    - **Bugs the first real jail launch surfaced** (each caught by the fail-closed
      probe; the errno=11 thesis in action — none were reachable by the mocked
      unit tests). *Code/target* bugs that would have broken the sandbox on
      Railway too, now fixed:
        1. seccomp policy used `umount2`, not a kafel amd64 identifier → policy
           never compiled; kafel names syscall 166 `umount`.
        2. `--uid_mapping`/`--gid_mapping` require the setuid `newuidmap`/
           `newgidmap` helpers (absent, unneeded) → switched to `--user`/`--group`
           (direct `/proc` self-map, `is_newidmap=false`).
        3. jail bind-mounts host `/` read-only then binds the workdir onto
           `/sandbox`, which didn't exist on the RO root → pre-create `/sandbox`
           in the image.
        4. runtime passed a bare `python3`; nsjail `execve()`s with no PATH search
           → resolve the interpreter to an absolute path at startup.
      Plus one *test* false-positive (LC_CTYPE, injected by CPython's PEP 538, was
      flagged as a host-env leak) and one *CI-host* relaxation (below).
    - **CI-host relaxation (containment):** nsjail's userns + mount setup is
      blocked on a stock `ubuntu-latest` (24.04) by three layers, so the smoke job
      clears exactly these — all on the **outer** container/host, none touching the
      inner jail, none adding capabilities, container still runs as non-root
      `runner`: `--security-opt seccomp=unconfined` (CLONE_NEWUSER unshare),
      `--security-opt apparmor=unconfined` (mount ops), and host
      `sysctl kernel.apparmor_restrict_unprivileged_userns=0` (Ubuntu 24.04 userns
      mount restriction — a kernel global a container flag can't lift). The
      passing escape corpus is the proof the jail is intact. See README → *Why the
      smoke container relaxes Docker's own sandbox*.

## CI gates — runner-smoke + escape corpus (plan §4.3 / §4.4 / §4.5)

First CI in the repo. `.github/workflows/ci.yml`:

- [x] **`test`** job — `go vet` + `go test -race ./...` (matches what passes locally).
- [x] **`runner-smoke`** job (§4.3) — build the image (nsjail from source) → boot
      under target-like limits with `RUNNER_SANDBOX=require` → wait `/healthz`
      (container-exit detection makes a failed fail-closed probe a loud job
      failure) → assert `nsjail ENABLED` in the boot log → real `POST /run`
      `print(2+2)` ⇒ `success` / `"4\n"`.
- [x] **Escape corpus** (§4.4) — `scripts/smoke-escape.sh`, now the **full §4.4
      table** (compiler bomb excepted — needs F-D): fork bomb → contained + host
      survives; secret/host-env read → denied; seccomp-killed syscall (`unshare`);
      output flood → truncated, no OOM; **egress + cloud-metadata → empty-netns
      blocked; CPU spin → `timeout`; memory bomb → `memory_exceeded` (RLIMIT_AS);
      host write (`/usr`,`/bin`,`/app`,`/sandbox`) → read-only denied**. Each
      asserted contained; a final host-survival run must still succeed.
- [x] **Stress / backpressure** (§4.4 tail, R5) — `scripts/smoke-stress.sh` +
      a dedicated `RUNNER_MAX_CONCURRENT_RUNS=1` container in CI: firing
      `cap+3` concurrent runs proves the excess is shed with `503` + `Retry-After`
      (not queued, no crash) and the runner still serves after the burst.
- [x] Helper `scripts/smoke-run.sh` — `POST /run`, exact stdout+status assertion,
      non-zero exit on mismatch (reused by the corpus + stress).
- [ ] Still open: `runner-security` as a *separate* periodic job, image CVE scan
      (trivy/grype), and the **compiler-bomb** corpus row — the last deferred with
      F-D (no compiled runtime yet).

Signed-off: `claude/dalivim-runner-ci-smoke-3sscm8` — 2026-07-09
(escape-corpus completion + backpressure gate: 2026-07-09).

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
| R2 | No seccomp filter | F-B ✅ closed (kafel denylist; CI escape corpus kills `unshare` via seccomp) |
| R3 | `/tmp` unbounded → host OOM | F-B ✅ closed (tmpfs `/tmp`; CI output-flood truncated, no OOM) |
| R4 | No `no_new_privs` | F-B ✅ closed (nsjail default; CI `RUNNER_SANDBOX=require` boot asserts jail engaged) |
| R5 | Runner has no backpressure (never emits 503) | F-A ✅ closed |
| R6 | `memory_exceeded` from fragile stderr substring | F-E/F-F |
