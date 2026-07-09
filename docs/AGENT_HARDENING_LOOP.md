# Agent Loop — Spec-Driven Hardening of `dalivim-runner`

> **What this is.** A standing operating procedure for an autonomous coding agent
> (Opus 4.8) to execute the runner hardening roadmap **one phase at a time, in a
> loop, driven by the spec**. The spec decides *what* to build; you decide *how*,
> strictly within the rules below. You do **not** invent scope, and you **never**
> weaken containment to make a step pass.
>
> **How to start the loop.** Point the agent at this file:
> *"Execute `docs/AGENT_HARDENING_LOOP.md`. Loop until you hit a STOP condition,
> then report."* (or drive it with the `/loop` skill). Each iteration ends with a
> pushed **draft PR** and a report; the loop then re-enters at step 1.

---

## 1. Authoritative spec — read at the start of every iteration

State is on disk, not in your memory. Re-read these each loop; do not trust your
recollection from a previous iteration.

| File | Role |
|---|---|
| `docs/RUNNER_ARCHITECTURE_AND_HARDENING_PLAN.md` | **THE spec.** ADRs (D-1…D-6), threat model (§2), per-phase design (§2/§3), roadmap + acceptance criteria (§5), deploy checklist (App. B). |
| `docs/RUNNER_HARDENING_CHECKLIST.md` | **The living tracker = your task queue and state.** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked. |
| `docs/RUNNER_AUDIT_AND_CONTRACT.md` | The wire contract + residual-risk register (R1…R6). |

**Precedence rule.** If code and the plan disagree, the plan wins — *unless the
plan itself is wrong or unsafe*, in which case you **STOP and escalate** (§6). You
never silently diverge from the spec.

**Trust-but-verify the tracker.** Checkboxes can be stale. Before selecting work,
confirm the real state from code + `go test`, not from the `[x]` marks alone.

---

## 2. Invariants — never violated; checked before every commit

These come straight from the plan's decisions and threat model. A change that
breaks any of them is rejected, even if tests are green.

1. **Contract stability (D-5).** `Gateway.Run` and the `RunnerProvider` interface
   are immutable. Growth is **additive**: new providers, new `Rule`s, new
   `languageSpec` entries, or already-reserved DTO fields (`Language`,
   `CompileOutput`, `Signal`, `CompileTimeoutMs`). Never repurpose or remove a
   wire field; keep deprecated aliases until every caller migrates.
2. **Fail closed.** Missing token → refuse boot (outside `development`).
   `RUNNER_SANDBOX=require` / `RUNNER_NETWORK_ISOLATION=require` → boot error when
   unavailable, **never** a silent downgrade. Unknown language → `400`. Unknown
   policy value → boot error.
3. **No global `RLIMIT_NPROC` (D-3 / the `errno=11` lesson).** Fork-bomb
   containment is **per-jail only** (`--rlimit_nproc` against a jail-private uid) +
   the bounded concurrency cap. Never reintroduce a process-wide nproc cap or
   otherwise starve the Go runtime's threads.
4. **Untrusted input never touches a command line.** Student source is written to
   a file (`0600`) and referenced by path; it is never interpolated into a shell
   or argv. The **compiler is untrusted too (D-4/§3.4)** → compile in a *separate*
   jail from execution; the run jail contains no toolchain.
5. **Jail integrity is sacred.** Never weaken containment to make a smoke/test
   pass — no dropping the seccomp policy, no `--disable_clone_newuser`, no writable
   rootfs, no re-enabling egress. A green result obtained by weakening the jail is
   a **regression**, not a fix. If containment relaxation is unavoidable at the
   *runner-container* layer (e.g. Docker seccomp for userns), it must not relax the
   *jailed run*, and you document exactly what was relaxed and why.
6. **The seam holds.** All execution flows through `sandbox.Sandbox.Command(Spec)`.
   Runtimes describe *what* to run (argv/workdir/limits); they hold **no** syscall
   or namespace code. New languages plug in as `languageSpec` entries + a `Runtime`,
   nothing more.
7. **Closed registries.** Adding a language is a deliberate edit to the
   `languageSpec` registry and the contract's language catalog — never dynamic,
   never from request input.
8. **Boot integrity over mocks (D-6).** Prove the **real binary/image** boots and
   runs under target-like limits (`--pids-limit`, `--cpus=1`, `--memory`,
   `GOMAXPROCS=1`). "Green with mocks" is not "boots on the target."
9. **Green before commit.** `gofmt -l` empty, `go vet ./...` clean,
   `go test -race ./...` green, and the code builds for `linux` *and* the non-Linux
   stub (`GOOS=darwin`). No exceptions.
10. **Git hygiene.** One phase (or one coherent sub-item) per branch off `main`.
    Draft PR at the end. **Never** merge, force-push, or push to `main`. Tick the
    checklist in the *same* PR as the code it describes.

---

## 3. The loop — one iteration

```
1. SYNC     git checkout main && git pull. Re-read the three spec docs (§1) and
            the checklist. Derive the TRUE current state from code + `go test`.
2. SELECT   Pick the next actionable item (§4). If none is actionable in this
            environment → STOP with the reason (§6).
3. PLAN     Restate the item's acceptance criterion (plan §5) and the exact files
            you will touch. If the design is ambiguous, or the plan looks wrong or
            unsafe → STOP and escalate (§6). Do not guess on security.
4. BRANCH   git checkout -b <phase-slug>   (e.g. f-c-node, f-e-smoke-ci).
5. BUILD    Smallest change that meets the criterion, obeying every invariant (§2)
            and the phase design in the plan (§2/§3). Reuse the existing seam.
6. TEST     Unit-test all pure logic (arg builders, registries, clamps, state
            machines). Skip-gate anything needing the jail/binary/userns. Extend
            the smoke script for jail/boot behaviour unreachable in CI. Run the
            §2.9 gates.
7. GATE     Re-read the acceptance criterion and DEMONSTRATE it is met — not just
            "tests pass." If you cannot demonstrate it here, the item is NOT done:
            mark `[~]` with what remains and which target step is required.
8. TRACK    Tick only the boxes you truly closed. Update residual-risk rows if a
            risk moved. Keep notes one line each.
9. SHIP     Commit (with the Co-Authored-By trailer), push, open a DRAFT PR that
            names the phase and quotes its acceptance criterion. Never merge.
10. REPORT  Emit the iteration report (§7). Then LOOP to step 1 — unless a STOP
            condition (§6) holds.
```

---

## 4. Phase selection

Work the checklist **top-down**, honoring the roadmap's dependency column
(plan §5 "Depende de"). Take the highest item that is both unchecked (`[ ]`/`[~]`)
and whose dependencies are `[x]`. One phase per iteration; split a large phase into
coherent sub-items (e.g. F-C = wire `/run` generic → then add the Node `Runtime`).

Roadmap and acceptance criteria (plan §5 — the definition of done):

| Phase | Deliverable | Depends on | **Acceptance criterion (the gate)** |
|---|---|---|---|
| F-A | Fix `errno=11` (automaxprocs, no global NPROC, boot smoke) | — | `[x]` done |
| Refactor | Sandbox → `Command(Spec)` seam | — | `[x]` done |
| F-B | nsjail (dial, probe, args, seccomp, Dockerfile) | F-A | `[~]` code done; **target-validate**: log `nsjail ENABLED` on the target + escape corpus contained |
| F-C | Interpreted polyglot (Node): generic `/run`, `languageSpec`, adapter drops the non-Python reject | F-B | **smoke JS green; JS inherits the jail** |
| F-D | Compiled polyglot (C/C++): compile-jail + run-jail, strict seccomp allowlist for the static binary | F-C | **correct `compile_error`; C escape corpus contained** |
| F-E | Continuous hardening: `runner-security` corpus in CI, trivy, stress/concurrency | F-B | **pipeline blocks any containment regression** |
| F-F | Router + Judge0 (only on real demand) | stable contract | **swap provider per language without touching the core** |

**Order:** F-B target-validation and **F-E** are the near-term work (F-E runs in
parallel from F-B and is the mechanism that *proves* F-B). Then F-C → F-D. F-F only
when the language matrix or load justifies a privileged VPS — until then it is
YAGNI (plan §1.4) and you do **not** build it.

**Environment reality (do not fight it).** nsjail needs its binary + unprivileged
user namespaces; neither exists in a plain `go test`. Jail-dependent criteria are
met via the Docker **smoke job** (§4.3 of the plan) on a runner that permits
userns, and/or a target verification deploy with `RUNNER_SANDBOX=require`. When a
criterion is only reachable on the target, do the CI-reachable part, mark `[~]`,
and name the exact target step in your report — that is progress, not failure.

---

## 5. The escape corpus (F-B validation / F-E gate)

Each adversarial submission **must** be contained (plan §4.4). This table is the
executable definition of "the jail works"; encode it in the smoke/security job.

| Attack | Submission (sketch) | Required containment |
|---|---|---|
| Egress | `socket.create_connection(("1.1.1.1",80))` | `Network is unreachable` |
| Cloud metadata | connect `169.254.169.254:80` | blocked |
| Fork bomb | `while True: os.fork()` | killed by nproc; host survives |
| CPU spin | `while True: pass` | `timeout` (wall/CPU) |
| Memory bomb | `b"x"*10**10` | `memory_exceeded` |
| Secret read | read `/etc/shadow`, host env | denied (ro rootfs, minimal env) |
| Host write | write `/usr`, `/app` | read-only ⇒ fails |
| Dangerous syscall | `ptrace`, `mount`, `unshare` | killed by seccomp |
| Output flood | `print("x"*10**9)` | truncated at the cap, no OOM |
| Compiler bomb (C++) | explosive template/macro | `compile_error` at the compile timeout |

Plus stress: N concurrent runs at `RUNNER_MAX_CONCURRENT_RUNS` → prove `503`
backpressure and no PID/tmp leakage between runs.

---

## 6. STOP / escalate conditions

Stop the loop and report — do not push forward — when:

- **Human/target gate.** The next item needs an action you cannot take: a Railway
  verification deploy, a privileged VPS (F-F), a secret, or CI infra you can't
  provision. (E.g. final F-B sign-off, or F-D on a host you don't have.)
- **Spec conflict / unsafe design.** The plan contradicts itself or reality, or a
  step as written would break an invariant or reduce containment.
- **Invariant collision.** You cannot satisfy the criterion without violating §2.
- **Unreachable-in-env criterion.** Do the CI-reachable slice, mark `[~]`, and
  state precisely what remains and where it must run.
- **No forward progress** across two consecutive iterations.
- **Roadmap exhausted.** All actionable phases are `[x]`; only human/target-gated
  or YAGNI (F-F) items remain.

Escalation is a first-class outcome. A clear STOP with the exact blocker beats a
speculative change that erodes the security posture.

---

## 7. Iteration report (emit at the end of every loop)

```
Phase/item:      <e.g. F-C — generic POST /run + Node runtime>
Criterion:       <quoted from plan §5>
Changed:         <files + one-line why each>
Verification:    gofmt ✓  vet ✓  test -race ✓  cross-build ✓
                 <which new tests; what is target-deferred and why>
Containment:     <invariants re-checked; any runner-container relaxation noted>
Checklist delta: <boxes ticked / moved to [~], with the remaining note>
PR:              <draft PR URL>
Next / STOP:     <next actionable item, or the STOP reason from §6>
```

---

## 8. First iteration — expected pick

At the time of writing: F-A `[x]`, Refactor `[x]`, **F-B `[~]`** (code done,
target-validation pending). Under §4, the highest actionable items are **F-E's
`runner-smoke`/`runner-security` CI** (which *validates* F-B against §5's corpus on
a userns-capable runner) and **F-C (Node)**. Prefer F-E first: it turns F-B's
`[~]` into a verifiable `[x]` and installs the regression gate that every later
phase leans on. Re-derive this from the live checklist — do not hard-code it.
