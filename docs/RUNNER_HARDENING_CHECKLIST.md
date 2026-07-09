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
| F-C | Interpreted polyglot (Node): `languageSpec` registry + JS runtime on the shared jail | [x] done (unit green; JS smoke + jail-inheritance gated in CI) |
| F-D | Compiled polyglot (C/C++): compile-jail + run-jail, `signal`, `compile_output` | [x] core done (two jails, static, minimal-rootfs run jail; CI smoke). Strict seccomp allowlist = follow-up |
| F-E / F-F | cgroups accounting, deterministic `memory_exceeded` classification | [x] R6 done (`RUNNER_CGROUP` v2 dial: per-run `memory.max`/`pids.max`, OOM-event classification; VPS-validated). F-F CPU accounting: todo |
| **G3** | Multi-file submissions (`files[]`) + traversal-resistant materialization | [x] runner side done (contract, `os.Root`/`openat2` writer, C/C++/Java/Go/Python/JS multi-file, adversarial path/symlink/limit suite green, multi-file CI smoke). Backend must send `files[]` — see [G3_MULTIFILE.md](./G3_MULTIFILE.md) |

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
- [x] **Image CVE scan** (§4.5c) — separate `security-scan` job builds the shipped
      image and runs `trivy image --severity HIGH,CRITICAL --ignore-unfixed
      --exit-code 1`. `--ignore-unfixed` keeps it actionable (fails only on a
      *fixable* HIGH/CRITICAL, never on an unpatched upstream CVE); scanning the
      shipped image covers the runtime + nsjail libs now and the Node/gcc
      toolchains F-C/F-D add.
- [x] **Periodic security gate** (§4.4 "rodado no CI e periodicamente") — a weekly
      `schedule` trigger re-runs both the escape corpus (via `runner-smoke`) and
      the CVE scan (`security-scan`), so base-image drift / post-merge CVEs surface
      without a code change. This is the "separate periodic job" for the security
      corpus: the corpus stays a per-PR gate in `runner-smoke` **and** fires on the
      weekly cadence, and `security-scan` is its own independent job.
- [~] **Compiler-bomb corpus row** (§4.4 last row) — deferred with **F-D**: needs a
      compiled runtime to produce `compile_error` at the compile timeout. Add the
      row to `smoke-escape.sh` (or a compiled-language corpus) in the F-D PR.

Signed-off: `claude/dalivim-runner-ci-smoke-3sscm8` — 2026-07-09
(escape-corpus completion + backpressure gate: 2026-07-09;
trivy CVE scan + weekly periodic gate: 2026-07-09).

## F-C — interpreted polyglot (Node)

Criterion (plan §5): **smoke JS green; JS inherits the jail.**

- [x] `languageSpec` registry (`internal/executor/languages.go`) — the closed
      extension point invariant §6/§7 calls for: `python` + `javascript` entries
      describing source filename, jail argv, env, version parsing, and the memory
      model. Adding an interpreted language is an entry here plus a constructor.
- [x] Generic `interpretedRuntime` (`interpreted.go`) replaces the hardcoded
      `PythonRuntime`: one spec-driven runtime both languages share, still routing
      every run through `sandbox.Command(Spec)` — no per-language syscall code.
      `NewPython`/`NewNode` register in the composition root.
- [x] Node runtime: `node --disable-proto=throw main.js`, single-file (no npm /
      node_modules), 0600 source written to a file (never the command line).
- [x] **RLIMIT_AS ≠ Node** (target-reality bug, D-6): V8 reserves a multi-GB
      virtual "cage" at startup, so a tight `RLIMIT_AS` (128–512MB) makes Node hang
      or fatally fail ("Failed to reserve virtual memory for CodeRange") — proven
      locally. Fix: `Spec.AddressSpaceMB=0` skips the address-space cap for Node
      (both backends), and the V8 heap is bounded by `--max-old-space-size=<budget>`
      instead; the container/cgroup memory limit is the RSS backstop (plan §2.3;
      per-jail cgroup `memory.max` is the F-E upgrade). Python keeps the hard
      `RLIMIT_AS` → deterministic `memory_exceeded`.
- [x] Adapter no longer Python-only: the generic `POST /run` already dispatches by
      `language` (`handlers.go`), so JS needed only a registered runtime.
- [x] Dockerfile installs `nodejs` (bookworm v18, supports `--disable-proto=throw`).
- [x] Unit tests (Node available → they RUN, else skip): success, stdin,
      runtime_error, timeout, `--disable-proto=throw` throws, version, jail
      inheritance (netns egress denied), per-language memory model, spec registry.
- [~] **CI smoke JS green** — added to `runner-smoke` (`console.log(2+2)` ⇒
      `success`/`"4\n"`) plus a jail-inheritance assertion (Node host-write to
      `/usr` denied by the read-only rootfs). Runs on GitHub Actions (Docker); this
      environment has no Docker, so the JS smoke is verified on the PR's CI run.

## F-D — compiled languages (C/C++)

- [x] Wire `compile_timeout_ms` (request) + `compile_output` / `signal` (result) +
      `StatusCompileError` in the contract.
- [x] `compiledRuntime` (`internal/executor/compiled.go`) behind the same
      `language` dispatch: **two jails** (D-4/§3.4). Phase 1 compiles in its own
      jail — full rootfs (the toolchain), a **writable** `/sandbox` for the
      artifact, `-static` link, separate compile CPU/mem/time budget
      (`RUNNER_COMPILE_*`); non-zero exit or compile timeout ⇒ `compile_error`,
      nothing runs; the artifact is size-validated. Phase 2 runs the static binary
      in a **minimal-rootfs** jail (new `Spec.MinimalRootfs`) — no libc, **no
      toolchain**, so the run cannot re-invoke the compiler or exec anything else.
- [x] Sandbox seam extended additively: `Spec.Writable` (rw `/sandbox` for compile)
      and `Spec.MinimalRootfs` (drop the host rootfs bind for the static run jail);
      `sandbox.JailPath` shares the mount path with runtimes. Arg-builder + `subst`
      + `signalName` unit-tested; `c`/`cpp` in `registry`. Dockerfile adds
      `gcc g++ libc6-dev`.
- [x] CI runner-smoke exercises the real two-phase path: C & C++ `2+2` ⇒
      `success`/`"4\n"`, a syntax error ⇒ `compile_error` (nothing executed), and
      `execl("/usr/bin/gcc")` from the run jail fails (no toolchain present).
- [x] **F-D follow-up — strict seccomp allowlist for the static run jail.** A kafel
      ALLOWLIST (DEFAULT KILL) of the minimal set a self-contained binary needs
      (read/write/mmap/brk/rt_sigreturn/exit_group/glibc-static startup/…),
      excluding `execveat`/socket/`open`/`openat`/`ptrace`/`clone`/mount — much
      tighter than the interpreter denylist. (`execve` is permitted because nsjail
      execve's the payload after installing the filter; neutered by the
      minimal-rootfs run jail — nothing else to exec, no `open`/`openat` to make
      one.) Behind `RUNNER_STATIC_SECCOMP=off|
      enforce|complain` (default off; `complain` = DEFAULT LOG for tuning the set on
      the target). Policy selection + banned/essential syscalls unit-tested; CI
      boots a dedicated `enforce` container and proves ordinary C **and** C++ still
      run under it (a SIGSYS kill would fail the gate). Set tuned on the target via
      `complain` before flipping prod to `enforce`. Java (javac+JVM) stays
      demand-gated (§3.4).

## F-E / F-F — accounting & classification

Fixes **R6** (fragile `memory_exceeded` substring heuristic).

- [x] cgroups v2 memory/pids accounting behind the `RUNNER_CGROUP=auto|require|off`
      dial (`internal/sandbox/cgroup_linux.go`). The runner self-manages each run's
      leaf cgroup — create → `memory.max`/`pids.max` → clone nsjail in via
      `CLONE_INTO_CGROUP` (`exec.Cmd.UseCgroupFD`) → read `memory.events`/`memory.peak`
      → remove — so the OOM counter is readable after the run (nsjail's own
      `--cgroup` flags delete the cgroup on exit, making it racy). `memory.peak`
      becomes the authoritative `memory_kb`. Boot probe proves the whole mechanism
      end-to-end (delegated-child create + limits + clone-into-cgroup); `require`
      fails closed, `auto` falls back to `RLIMIT_AS`. Dial + parsers unit-tested;
      cgroup execution is target-validated (no delegated cgroup in `go test`/CI).
- [x] Classify `memory_exceeded` from the cgroup OOM event (`memory.events`
      oom_kill/oom_group_kill), not stderr text — authoritative when a cgroup is
      present; the stderr substring remains only as the no-cgroup fallback. This
      also gives **Node** a real memory bound (`memory.max` counts RSS, which
      `RLIMIT_AS` can't apply to V8's virtual cage).
- [x] Validated on the production VPS: cgroup v2 with `memory`+`pids` delegated to
      the runner uid; delegation + `child_mkdir`/`memory.max`/`pids.max` proven.
      Deploy runbook + throwaway validation in `docs/DEPLOY.md` §8b. *(nsjail
      ENABLED already confirmed in prod — F-B target-validated.)*
    - **Deploy findings (VPS):** (a) `CLONE_INTO_CGROUP` needs write on the common
      ancestor of the runner's own cgroup and the target leaf, so the container
      must be **born inside** the delegated subtree (`--cgroup-parent`) — chowning
      the leaf alone yields `EPERM`; and the systemd cgroup driver fights a
      hand-`mkdir`'d subtree (use the cgroupfs driver or a delegated `.slice`). The
      boot probe catches this end-to-end and `require` fails closed; its error now
      names the runner's own cgroup and the `--cgroup-parent` remedy. (b)
      `/sys/fs/cgroup` is **tmpfs** — the delegated subtree vanishes on reboot, so
      `require` + `--restart` without a boot-time recreate = the runner fails
      closed and stays down after a reboot. §8b now ships a `dalivim-cgroup.service`
      systemd oneshot (`Before=docker.service`) and mandates it **before** flipping
      `require`; ship prod on `auto` until then.
- [ ] F-F: per-run CPU accounting (`cpu.stat`) is future work; not needed for R6.

---

## Residual-risk index

| ID | Risk | Addressed by |
|---|---|---|
| R1 | Global `RLIMIT_NPROC` → auto-DoS (`errno=11`, **confirmed**) | F-A ✅ closed |
| R2 | No seccomp filter | F-B ✅ closed (kafel denylist; CI escape corpus kills `unshare` via seccomp) |
| R3 | `/tmp` unbounded → host OOM | F-B ✅ closed (tmpfs `/tmp`; CI output-flood truncated, no OOM) |
| R4 | No `no_new_privs` | F-B ✅ closed (nsjail default; CI `RUNNER_SANDBOX=require` boot asserts jail engaged) |
| R5 | Runner has no backpressure (never emits 503) | F-A ✅ closed |
| R6 | `memory_exceeded` from fragile stderr substring | F-E ✅ closed (cgroup v2 OOM event via `RUNNER_CGROUP`; stderr only the no-cgroup fallback) |
