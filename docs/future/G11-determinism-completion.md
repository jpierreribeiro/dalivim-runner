# G11 — Determinism completion (hash seed, PRNG, per-language nondeterminism)

> **Status — ✅ implemented (2026-07-11).** `PYTHONHASHSEED=0` is pinned on the
> Python run env (`internal/executor/languages.go`), and the interpreter now
> launches with `-s -P` instead of `-I` — **discovered in implementation**: `-I`
> (isolated mode) implies `-E`, which makes CPython ignore every `PYTHON*` var,
> silently defeating the seed pin; `-s -P` preserves the user-site / safe-`sys.path`
> hardening while letting the seed take effect (the run env is fully controlled, so
> dropping `-E` costs no isolation). The other languages were audited — Go map order
> is randomized by design and not seedable, JS/Java/Lua expose no equivalent knob —
> and `docs/DETERMINISM.md` now carries the per-language guarantees + non-guarantees.
> A behavioral test proves Python set-iteration output is byte-identical across two
> runs. Planning notes kept below.

A small, high-value follow-up to [G7](G7-execution-determinism.md). G7 pinned the
*environment* (`LANG`/`LC_ALL`/`TZ`); G11 closes the remaining **language-level**
sources of run-to-run nondeterminism that G7 explicitly left to the student/backend
but that are cheaply fixable at the runner for the ones that are pure environment
knobs. It becomes load-bearing the day exact-match or test-based grading (G9) ships.

> **Scope discipline.** The runner pins a stable *environment*, not stable
> *programs* (`G7-execution-determinism.md:76-78`). G11 only pins the
> nondeterminism that is an **environment/interpreter switch** (hash seed, unbuffered
> I/O) — never program logic (`time.Now()`, unseeded PRNG *inside* the program, Go
> map iteration order the language randomizes by design). Those stay the student's
> concern; G11 just documents them honestly.

---

## Motivation

G7 made locale/TZ policy. But the *same submission* can still produce different bytes
across runs for reasons G7 didn't cover:

- **CPython hash randomization.** Since 3.3, `str`/`bytes`/`datetime` hashing is
  seeded randomly per process (`PYTHONHASHSEED` unset ⇒ random), so `set`/`dict`
  **iteration order** varies run to run. A program printing `set(...)` or iterating a
  `dict` built from strings can emit different byte order each time — a silent
  exact-match coin flip. The runner sets `PYTHONUNBUFFERED=1` today but **not**
  `PYTHONHASHSEED` (`internal/executor/languages.go:108`).
- **Unseeded PRNGs and wall-clock** are genuinely the program's nondeterminism and
  stay out of scope — but the docs should say so explicitly so a grader doesn't file
  a bug against the runner.

This is the cheapest correctness win left: a couple of env vars and a doc paragraph.

## Current state

- `determinismEnv` (`internal/executor/languages.go:19-21`) prepends
  `LANG=C.UTF-8 LC_ALL=C.UTF-8 TZ=UTC` to every spec's run env — the G7 pin.
- **Python** (`languages.go:99-121`): `env` = `determinismEnv("PATH=…",
  "PYTHONUNBUFFERED=1")`. **No `PYTHONHASHSEED`.**
- **JS / Lua / C / C++ / Go / Java**: env is `determinismEnv(...)` + per-language
  extras (`GOMAXPROCS=1` for Go, `GLIBC_TUNABLES=…rseq=0` for C/C++). No hash-seed
  knob is relevant (or exposed) for these.
- `docs/DETERMINISM.md` documents what is pinned; it does **not** yet call out hash
  randomization or list the explicit non-guarantees per language.

## Proposed change

1. **Pin `PYTHONHASHSEED=0`** on the Python spec's env (`languages.go:108`) — via
   `determinismEnv("PATH=…", "PYTHONUNBUFFERED=1", "PYTHONHASHSEED=0")`. Makes
   set/dict iteration order stable across runs. It's a pure environment switch (no
   program change), exactly the class G7 pins.
2. **Audit every language for the same class of knob** and pin what exists:
   - **Node**: hash order is not env-seedable and V8 object key order is
     insertion-ordered already (stable) — nothing to pin; document it.
   - **Go**: map iteration is randomized **by design** and is *not* seedable — this is
     program-level and stays the student's concern (sort before printing). Document,
     don't fight it.
   - **Java**: `HashMap` order is unspecified but deterministic per JDK; no seed. If a
     future exercise needs it, that's `LinkedHashMap`/`TreeMap` in the student code —
     document.
   - **Lua**: table iteration via `pairs()` is unspecified order; program-level,
     document.
3. **Expand `docs/DETERMINISM.md`** into a per-language table: for each language,
   *what the runner pins* (env) vs *what remains the program's responsibility* (map
   order, PRNG seed, `time.Now()`, float formatting across archs). One honest table
   so a grader knows exactly where the line is.
4. **Optional (ties into G4):** surface the pinned determinism env on `/readyz`
   (`internal/transport/httpapi/handlers.go:306-325`) so the backend can assert the
   contract programmatically before it starts comparing output.

## Contract / config impact

- **No wire-contract change.** Behavioural: Python set/dict output becomes stable
  across runs.
- **One-time documented shift**, like the G7 locale pin: a Python program whose
  output currently depends on the *random* hash seed will change to a **fixed** order
  once — run the full suite to catch any test that was silently depending on today's
  randomness. Deterministic and one-time.

## Security considerations

Neutral-to-positive, identical to G7 (`G7-execution-determinism.md:91-94`): pinning
`PYTHONHASHSEED` **removes** a degree of freedom. Note the historical reason the seed
is randomized — hash-flooding DoS on dict construction — is **not** a concern here:
the run is already CPU/wall/memory-bounded and single-shot, so a worst-case-collision
input hits the existing timeout, not an unbounded hang. Document that reasoning so
the pin isn't "fixed" back later by someone recalling the CVE.

## Testing

- **Python hash-order stability:** a program printing `set("...")` / iterating a
  string-keyed `dict` → **byte-identical** output across repeated runs (fails today
  intermittently, passes after the pin).
- **Regression:** the full suite green after the pin (catches any output that shifted
  from random to fixed order).
- **Doc accuracy:** each language's documented "not pinned" item is demonstrated
  once (e.g. Go map order differs run-to-run — proving the doc is honest, not that
  it's a runner bug).

## Effort

**XS.** One env var, an audit pass, and a doc table. The only care needed is the
one-time output-shift review and the DoS-reasoning note so the pin sticks.

## Phase G11 acceptance

- `PYTHONHASHSEED=0` is pinned; Python set/dict iteration output is stable run-to-run.
- Every language audited; each pinnable env knob is pinned, each program-level
  non-guarantee is documented in `docs/DETERMINISM.md` as a per-language table.
- The existing suite is green after the pin; a CI step asserts Python set-iteration
  output is byte-identical across two runs of the same submission.
