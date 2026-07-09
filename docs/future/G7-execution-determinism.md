# G7 — Execution determinism (locale / timezone / env)

The backend grades by **comparing stdout to expected**. Anything non-deterministic
in the runner's execution environment silently corrupts that comparison — the same
code produces different output across runs or across languages, and a student gets
a false WA (or a false AC). This phase closes the two live gaps — **locale** and
**timezone** — both currently unpinned. It's the cheapest high-value item in the
roadmap: no contract change, a handful of env vars, and a documented guarantee.

---

## Motivation
A program that prints a date, formats a number, or sorts accented strings behaves
differently under different locales/timezones. If the runner doesn't pin those,
the *same submission* can produce different bytes on different days (base-image
change) or in different languages (Python vs C) — turning the backend's exact-match
comparison into a coin flip. Determinism is a **correctness** property for a judge,
not a nicety.

## Current state
The environment is already **cleared, not inherited** — a security property that
holds today and must be preserved:
- nsjail passes `--keep_env` (`internal/sandbox/nsjail.go:166`), which combined with
  an explicitly-set minimal `cmd.Env` means "the child sees exactly the tiny env
  the caller set" — no host environment leaks in.
- Interpreted: `cmd.Env = r.spec.env` (`internal/executor/interpreted.go:118`).
- Compiled run jail: `cmd.Env = r.spec.runEnv`, forced to `[]string{}` when nil so
  the child inherits nothing (`internal/executor/compiled.go:468-470`).

But **`LANG` / `LC_*` / `TZ` are never set anywhere** (confirmed repo-wide). What
the student code actually sees today:
- **Python** (`internal/executor/languages.go:77`): `PATH`, `PYTHONUNBUFFERED=1`.
  CPython then coerces itself to a UTF-8 locale via PEP 538
  (`scripts/smoke-escape.sh:70-73` notes this).
- **JavaScript** (`languages.go:96`): `PATH` only.
- **C / C++** (`compiled.go:105`, `:119`): `GLIBC_TUNABLES=glibc.pthread.rseq=0`
  only (static binary, no PATH).
- **Go** run (`compiled.go:155`): `GOMAXPROCS=1` only.
- **Java** run: `runEnv` unset → `[]string{}` (empty).
- **Dockerfile** sets only `PATH` (`Dockerfile:69`), **no** `ENV LANG`/`TZ`.

**Net effect:** locale differs **by language** (CPython coerces to UTF-8; Node,
the JVM, and C/C++ land in the C/POSIX locale), and **`TZ=UTC` holds only by
accident** — the base image has no `/etc/localtime`, so it defaults to UTC; a base
change silently breaks it. Neither is policy.

## Proposed change
1. **Pin locale + timezone identically for every runtime.**
   - Dockerfile: `ENV LANG=C.UTF-8 LC_ALL=C.UTF-8 TZ=UTC` (verify `C.UTF-8` exists
     in the base image — it's built into modern glibc and the musl images; no
     locale package needed).
   - **Add `LANG=C.UTF-8`, `LC_ALL=C.UTF-8`, `TZ=UTC` to every run env** —
     `spec.env` / `runEnv` for python, js, c, cpp, go, **and** java. This is the
     load-bearing subtlety: because the run jail uses an **explicit minimal
     `cmd.Env`** (not the process environment), a Dockerfile `ENV` does **not**
     propagate through `--keep_env`. Setting it only in the Dockerfile is **not
     enough** — it must be in each spec's env slice.
   - Choose **`C.UTF-8`** (not `en_US.UTF-8`): locale-independent collation, UTF-8
     encoding, present without installing locale data. Document the choice.
2. **Document the determinism guarantees** (a "Determinism" section in TESTING.md
   or a dedicated `docs/DETERMINISM.md`): state exactly what the runner pins —
   `LANG`/`LC_ALL`/`TZ`, a fixed minimal env, `GOMAXPROCS=1` — and, honestly, what
   it does **not** control: language-level nondeterminism (Go map-iteration order,
   hash ordering, `time.Now()`, unseeded PRNGs, floating-point across archs). Those
   are the student's/backend's concern; the runner guarantees a stable
   *environment*, not stable *programs*.
3. **Optional**: surface the pinned env in `/readyz` (G4) so the backend can assert
   the determinism contract programmatically.

## Contract / config impact
- **No wire-contract change.** Behavioural: student code sees a consistent locale
  and timezone across all languages.
- Env additions are per-spec; a base-image `ENV` as belt-and-suspenders.
- **One-time documented shift**: pinning `LANG=C.UTF-8` can change output for
  programs that currently rely on CPython's PEP-538 coercion or an accidental
  locale. It's deterministic and one-time — run the full suite to catch any test
  that was silently depending on today's inconsistent behaviour.

## Security considerations
Neutral-to-positive. The env stays minimal and explicit (no host inheritance —
the existing property is preserved). Pinning **removes** an environmental degree
of freedom a student could lean on for nondeterministic behaviour. No new surface.

## Testing
- **Cross-language identity**: a program that prints `locale`, the current
  timezone, a formatted date, and a locale-sensitive sort → **identical** output
  across python/js/c/cpp/go/java.
- **Regression**: the full existing suite stays green after the pin (catches any
  output that shifted).
- **TZ**: a program formatting a fixed epoch as local time → UTC, every language;
  assert `TZ=UTC` is set **explicitly**, not inherited from a missing
  `/etc/localtime`.

## Effort
**XS/S.** Dockerfile `ENV` + three vars added to each spec's env slice + a
determinism doc. The only trap is the per-spec requirement (Dockerfile alone is
insufficient).

## Phase G7 acceptance
- `LANG`/`LC_ALL`/`TZ` are explicit and identical across all six languages' run
  environments.
- The determinism guarantees are documented — what is pinned, and what is
  explicitly *not* the runner's job.
- The existing suite is green after the pin.
