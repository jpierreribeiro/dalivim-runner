# Execution determinism (G7)

The runner guarantees a **stable execution environment**: the same submission,
run twice — in any language, on any deploy of the same runner version — sees the
same locale, timezone, and environment. This matters wherever output is compared
or diffed (a judge, a regression suite, a cached result); without it, the same
program can produce different bytes depending on the language runtime's locale
defaults or a base-image change.

## What the runner pins

Every run jail — interpreted and compiled, all six languages — receives an
**explicit, minimal environment**. Nothing is inherited from the host process
(nsjail's `--keep_env` combined with an explicitly-set `cmd.Env` means the child
sees exactly the slice the spec declares). On top of each language's minimal
extras, every run env carries the shared determinism pin
(`determinismEnv` in `internal/executor/languages.go`):

| Variable | Value | Why |
|---|---|---|
| `LANG` | `C.UTF-8` | UTF-8 encoding with locale-independent collation; built into modern glibc, no locale data needed |
| `LC_ALL` | `C.UTF-8` | overrides every `LC_*` category, so no category can drift individually |
| `TZ` | `UTC` | timezone as policy — previously UTC only because the base image happens to lack `/etc/localtime` |

`C.UTF-8` is deliberate over `en_US.UTF-8`: collation and number/date formatting
are locale-independent (POSIX rules), the encoding is UTF-8, and it exists in
glibc and musl without installing locale packages.

The per-language envs, in full:

| Language | Run environment |
|---|---|
| python | pin + `PATH`, `PYTHONUNBUFFERED=1` (multi-file adds `HOME=/nonexistent`) |
| javascript | pin + `PATH` (multi-file adds `HOME=/nonexistent`) |
| c / cpp | pin + `GLIBC_TUNABLES=glibc.pthread.rseq=0` |
| go | pin + `GOMAXPROCS=1` |
| java | pin only |

Other pinned properties that contribute to run-to-run stability:

- **`GOMAXPROCS=1`** for Go runs (single OS thread for the scheduler).
- **Fixed minimal env** — the set above is exhaustive; a host env var can never
  leak into a run and change behaviour.
- The Dockerfile also sets `ENV LANG=C.UTF-8 LC_ALL=C.UTF-8 TZ=UTC` as
  belt-and-suspenders for the daemon and image tooling — but note this **cannot**
  substitute for the per-spec pin: the jail's explicit `cmd.Env` means a
  process-level ENV never reaches the child.

## What the runner does NOT control

The runner guarantees a stable *environment*, not stable *programs*.
Language-level nondeterminism remains the submission's (or the backend's)
concern:

- Go map-iteration order; hash ordering in any language.
- `time.Now()` / `Date.now()` / wall-clock reads (the *timezone* is pinned; the
  clock still advances).
- Unseeded PRNGs (`math/rand`, `Math.random()`, `random` without a seed).
- Floating-point differences across CPU architectures.
- Thread/goroutine interleaving (mitigated but not eliminated by
  `GOMAXPROCS=1` / `ActiveProcessorCount=1`).

## History

Before G7, locale differed **by language**: CPython coerced itself to a UTF-8
locale via PEP 538, while Node, the JVM, and C/C++ landed in the C/POSIX locale;
and `TZ=UTC` held only by accident of the base image. Pinning `LANG`/`LC_ALL`
explicitly is a one-time, documented behavioural shift for any program that was
silently depending on that inconsistency.
