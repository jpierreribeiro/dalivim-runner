# S2 — Tighten interpreters toward a seccomp allowlist (nsjail/kafel bump)

> **Status — ✅ implemented (2026-07-11).** Steps 1 and 2 are done. **Step 1 (the
> gate):** nsjail bumped `3.4 → 3.6` (`Dockerfile`), whose bundled kafel (submodule
> commit `76d0f41`) NAMES `io_uring_setup/enter/register` (425-427) and `userfaultfd`
> (323); build deps unchanged (3.6's pasta embedding is opt-in via `EMBED_PASTA`,
> unset). Verified before touching the runner's policy by compiling the exact extended
> denylist with that kafel — it produced a valid BPF program, and, installed as a live
> seccomp filter, killed all four syscalls with SIGSYS while benign syscalls survived.
> **Step 2:** `seccompPolicy` (`internal/sandbox/nsjail.go`) now KILLs those four;
> `clone`/`clone3` stay ALLOWED. **Validation:** the boot probe compiles the extended
> policy for every posture under `RUNNER_SANDBOX=require`, and new on-target smoke
> steps (`.github/workflows/ci.yml`) assert an `io_uring_setup` and a `userfaultfd`
> program each die with `SIGSYS` under the denylist, with all seven languages' happy
> paths + the escape corpus still green. **Step 3** (re-open the C/C++ allowlist's
> `statx`/`rseq` workarounds now that kafel can name them) is left as a documented,
> independently-validated follow-on — see the note in `nsjail.go` and §3 below.

The interpreted and VM runtimes run on a seccomp **denylist** (`DEFAULT ALLOW`, kill
the dangerous few); the static C/C++ run jail already runs on a tight **allowlist**
(`DEFAULT KILL`). This spec is the disciplined path to narrow the denylist —
including finally blocking `io_uring`/`userfaultfd` — which today is **blocked by a
tooling constraint**, not by choice: nsjail 3.4's bundled kafel cannot name the
syscalls we'd add. So S2 is really *"bump nsjail/kafel, then tighten."*

> **Why this is its own spec and not a one-line diff.** Adding `io_uring_*` to the
> denylist by name **fails the kafel policy compile** on nsjail 3.4 (its syscall
> table predates them), and under `RUNNER_SANDBOX=require` a failed policy compile
> **fails the boot probe closed** (`internal/sandbox/sandbox_linux.go:60-62`). The
> repo already documents this exact class of trap (`umount` vs `umount2`,
> `newfstat`/`newuname`, and the `rseq`/`statx` naming gap) in
> `internal/sandbox/nsjail.go:20-24,55-62`. So the change is real work with real
> rollout risk, gated on a toolchain bump — not a quick tightening.

---

## Motivation

The denylist is a deliberate, documented pragmatic choice: *"CPython and Node have an
enormous syscall surface that an allowlist breaks easily, so we start by killing the
syscalls a sandbox escape actually needs"* (`internal/sandbox/nsjail.go:9-13`). It
blocks `ptrace`, `mount`/`umount`/`pivot_root`/`chroot`, module loading, `bpf`,
`setns`/`unshare`, key management, `reboot`/`swap`, and the fd-to-path handle tricks
(`nsjail.go:25-36`). It does **not** block:

- **`io_uring_setup`/`io_uring_enter`/`io_uring_register`** — a repeated source of
  Linux LPE, and a bypass channel for other seccomp filters (I/O submitted via a ring
  isn't a syscall per op). No judged program needs it.
- **`userfaultfd`** — a classic exploit primitive (races kernel page faults). No
  judged program needs it.
- The `clone`/`clone3` family is intentionally allowed (glibc thread creation,
  `nsjail.go:15-18`) and must stay.

Blocking the first two is pure upside for judged code — **once the tooling can name
them**. It also moves the denylist closer to the allowlist model the C/C++ jail
already proves out.

## Current state

- Denylist policy string: `seccompPolicy` (`internal/sandbox/nsjail.go:25-36`),
  installed via `--seccomp_string` for every non-static run (`nsjail.go:167`).
- Static allowlist for C/C++: `staticAllowSyscalls` (`nsjail.go:71-81`), selected by
  `SeccompStaticEnforce`; `complain`-mode tuning already exists
  (`nsjail.go:83-101`, `cmd/runner/main.go:118-133`, DEPLOY §8c).
- **The tooling constraint, in the code's own words** (`nsjail.go:55-62`): *"nsjail
  3.4's bundled kafel is old enough to lack the NAMES of the newest syscalls
  (statx=332, rseq=334) and its grammar rejects bare numbers, so those two cannot be
  allow-listed at all."* `io_uring_*` are 425–427 and `userfaultfd` is 323 — the
  first three are **newer** than the ones kafel already can't name, so they cannot be
  added to a kafel policy on 3.4 today.
- nsjail is pinned and built from source at tag `3.4`
  (`Dockerfile:38-46`, `ARG NSJAIL_VERSION=3.4`).
- The boot probe compiles the real policy and fails closed on rejection
  (`internal/sandbox/sandbox_linux.go:298-324`), and CI's `runner-smoke` proves the
  jail on the shipped image (`ci.yml:45-124`).

## Proposed change

Three ordered steps; do **not** skip step 1.

1. **Bump nsjail (and thus kafel) to a version whose syscall table names `io_uring_*`,
   `userfaultfd`, `statx`, and `rseq`.** Update `ARG NSJAIL_VERSION`
   (`Dockerfile:39`) and the ABI-matched build deps if needed
   (`Dockerfile:40-46`). Verify the new kafel accepts the names in a throwaway policy
   before touching the runner's policy string. This is the gating dependency for
   everything else.
2. **Add the denials to `seccompPolicy`** (`nsjail.go:26-34`) — extend the `KILL`
   block with `io_uring_setup, io_uring_enter, io_uring_register, userfaultfd`. Keep
   `clone`/`clone3` allowed. The boot probe (`sandbox_linux.go:298`) will now *prove*
   the extended policy compiles; under `RUNNER_SANDBOX=require` a naming mistake fails
   the container closed — which is the safety net, but is exactly why step 1 must land
   and be verified first.
3. **Opportunistically re-evaluate the C/C++ allowlist gap the bump closes**
   (`nsjail.go:55-62`): with a kafel that names `statx`/`rseq`, the workarounds
   (`GLIBC_TUNABLES=…rseq=0`, relying on `newfstat`) can be revisited — smaller, not
   the point of S2, but a natural follow-on.

Optionally, longer-term: **prototype an interpreter allowlist under `complain` mode**
(the mechanism already exists, `SeccompStaticComplain`, `nsjail.go:96-98`). Run
CPython/Node representative programs, read `sudo dmesg | grep -i seccomp` for
`syscall=NNN` (the CI enforce job already dumps these, `ci.yml:501-508`), and see how
close a *default-KILL* interpreter policy is to feasible. This may prove intractable
(the whole reason for the denylist) — but `complain` mode makes the assessment
**data-driven** and free of risk, and even a partial result informs the posture.

## Contract / config impact

- **None on the wire.** The seccomp policy is internal. Behavioural: a (nonexistent
  in judged code) `io_uring`/`userfaultfd` call now dies with `SIGSYS` instead of
  running — surfaced via the existing `signal` field (`SIGSYS`, `compiled.go:685`).
- Image change: a newer pinned nsjail tag (and any ABI-matched dep bumps), refreshed
  per the Dockerfile's documented pin-refresh procedure (`Dockerfile:10-19`).

## Security considerations (of doing the hardening)

- **The failure mode is fail-closed and loud.** A wrong syscall name → policy compile
  fails → boot probe fails → under `require` the container won't start (`ci.yml`
  `runner-smoke` goes red, `sandbox_linux.go:60-62`). So the risk is a *broken
  deploy*, never a *silently weakened jail*. That's the right failure direction, but
  it means S2 **must** be validated by the on-target smoke before it reaches prod —
  never merged on unit tests alone (the repo's core lesson, `ci.yml:4-7`).
- **Don't over-tighten by reflex.** Killing a syscall glibc/CPython/V8 actually use at
  startup breaks every run of that language (`SIGSYS` at launch). The denylist is
  wide *because* the interpreter surface is wide; only add syscalls that are
  unambiguously escape-only (`io_uring`, `userfaultfd` qualify — thread/mmap/futex do
  not). Validate each addition against all interpreted + VM languages in CI.
- **Keep `clone`/`clone3` allowed** — removing them breaks modern glibc thread
  creation (`nsjail.go:15-18`); fork-bomb containment is `--rlimit_nproc` + the
  concurrency cap, not a clone block.

## Testing

- **Boot probe green** on the new nsjail for every posture (denylist runs, static
  enforce runs) — the probe compiling the extended policy is the first proof.
- **New denials bite:** a C program calling `io_uring_setup` (or `userfaultfd`) is
  killed with `SIGSYS`, not run to success — asserted on-target exactly like the
  existing `socket`-under-enforce check (`ci.yml:512-519`).
- **No regression:** the full `runner-smoke` + escape corpus + all seven languages'
  happy paths stay green on the bumped nsjail (proof the added denials don't touch a
  syscall a real interpreter/VM needs).
- **`complain` prototype (if pursued):** a dmesg capture of the interpreter syscall
  set, archived, informing whether an allowlist is viable — no enforcement change.

## Effort

**M/L.** Small diff, but gated on an nsjail/kafel bump with ABI-matched deps and a
**mandatory** on-target validation cycle; the optional interpreter-allowlist
assessment is the L part and may end in "denylist stays, documented why."

## Phase S2 acceptance

- nsjail/kafel bumped to a version that names `io_uring_*`/`userfaultfd` (verified in
  a throwaway policy before touching the runner's).
- `seccompPolicy` kills `io_uring_setup/enter/register` and `userfaultfd`; the boot
  probe compiles it and an on-target test proves a call to them dies with `SIGSYS`.
- All seven languages + the escape corpus stay green — no interpreter/VM regression.
- Any interpreter-allowlist assessment is data-driven (complain-mode dmesg), and its
  outcome (feasible-and-adopted, or infeasible-and-documented) is recorded.
