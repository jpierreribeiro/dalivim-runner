# G10 — SQL execution (SQLite first, PostgreSQL as a later shape)

> **Status — 🟡 phase 1 implemented: SQLite (2026-07-11).** `language:"sql"` runs a
> script against an in-memory SQLite (`sqlite3 :memory:`) and returns the result set
> as a **pinned, deterministic JSON row array** on stdout (`result_format:
> "json-rows"`, a new additive result field). SQLite is Shape A — an interpreter
> over `main.sql` — so it reuses the identical interpreted jail
> (`internal/executor/languages.go: sqliteSpec` + `NewSQL`); the only new thing is
> the pinned result format. The argv is fixed: `-batch -init /dev/null -bail -json`,
> `:memory:`, `.read main.sql`. **Discovered in implementation (verified against
> sqlite 3.45):** (1) the CLI enables `load_extension()` by **default**, so it is
> turned off with `.dbconfig load_extension off` — and that command **echoes its
> setting to stdout**, which would corrupt the JSON, so it is wrapped in
> `.output /dev/null … .output` to swallow the one line; (2) `-bail` makes a SQL
> error fail-fast (exit 1 → `runtime_error`) instead of running on to emit partial
> rows. **Scope:** source_code-only — a `files[]` sql request is a clean 400 (no sql
> file policy), because SQLite `.read` is cwd-relative and multi-file composition is
> deferred rather than shipped half-formed. Containment reuses the shared jail (no
> new surface, per this spec); the SQL-specific pieces (pinned format, error
> classification, extension-load disabled, timeout bound) get on-target smokes in
> `ci.yml`. Image: `sqlite3` pinned. Usage guide: `docs/SQL.md`. **Deferred:** phase
> 2 PostgreSQL (socket-orchestrated) and multi-file SQL.

SQL is one of the most-taught practical skills and a natural fit for an execution
judge — *given a schema + seed + query, run it and return the result set*. It stays
on the **executor** side: the runner runs the SQL against an **ephemeral, throwaway**
database and returns the rows **raw**. It never compares result sets, never judges.

> **The line that decides everything.** Two readings of "SQL support":
> 1. **Run SQL against a disposable in-process DB** (schema+seed+query → rows) — the
>    exercise model. ✅ Fits the stateless, one-shot, no-egress, no-persistence
>    runner.
> 2. **Connect to a real/persistent/networked database.** ❌ Breaks the empty netns,
>    needs shared state and a long-lived server. **Out of scope, permanently.**
>
> G10 is strictly (1).

---

## Motivation

The runner today executes *programming languages*. SQL is a different execution
model — a query engine over a dataset — but the same request/response shape works:
the "program" is a SQL script (DDL + DML + `SELECT`), the "output" is the result
set. Adding it lets the platform teach and grade SQL without a second service and
without a persistent database (which would be a security and ops liability).

The design choice is **which engine**, and the answer is staged: **SQLite now**
(serverless, in-process, zero startup — fits the model almost like an interpreted
language), **PostgreSQL later** and only if the PG dialect is genuinely required
(window functions, `generate_series`, `JSONB`, rich types), as a distinct runtime
shape.

## Current state

- Languages are a **closed registry** of interpreted (`languageSpec`,
  `internal/executor/languages.go:33-95`) and compiled (`compiledLangSpec`,
  `internal/executor/compiled.go:23-105`) shapes. `sql` is neither today.
- An interpreted language is `{name, binNames, runArgs, env, versionArgs, …}` +
  a constructor registered in `main.go` `NewService` (`cmd/runner/main.go:60-66`).
- `FilePolicy` (`internal/executor/filepolicy.go:112-159`) is the per-language file
  allowlist; there is no `sql` entry.
- Output is captured stdout/stderr with a per-stream cap (`internal/executor/buffer.go`);
  there is no notion of a **tabular result set** or its formatting.
- The empty network namespace (`internal/sandbox/nsjail.go:123`, `--iface_no_lo`)
  denies egress — and, crucially, **`AF_UNIX` sockets still work inside it**
  (netns isolates network sockets, not Unix domain sockets), which is what makes a
  socket-based engine (PostgreSQL, later) feasible in the jail at all.

## Proposed change — Phase 1: SQLite (the recommendation)

SQLite is **serverless and in-memory** (`sqlite3 :memory:`): no daemon, no socket,
no data directory, instant startup. Mechanically it slots into **Shape A**
(`ADDING-A-LANGUAGE.md:20-43`) — it *is* an interpreter reading a source file — with
one addition that is genuinely new: **result-set output formatting**.

### Spec

```go
var sqliteSpec = languageSpec{
    name:       "sql",              // wire id; the engine is an implementation detail
    sourceFile: "main.sql",
    binNames:   []string{"sqlite3"},
    // Run the script against a fresh in-memory DB. Formatting is pinned for
    // determinism (see below): a stable column mode, headers on, NULL rendered
    // as a fixed sentinel. -batch = non-interactive; -init /dev/null = no ~/.sqliterc.
    runArgs:    []string{"-batch", "-init", "/dev/null", ":memory:"},
    env:        determinismEnv("PATH=/usr/local/bin:/usr/bin:/bin"),
    capAddressSpace: true,          // sqlite's allocator is bounded by RLIMIT_AS
    memErrSubstr:    "out of memory",
    versionArgs:     []string{"-version"}, // "3.45.1 2024-..."
    parseVersion:    firstField,
}
```

The SQL script is fed on **stdin** (`sqlite3 … :memory:` reads statements from
stdin), or written to `main.sql` and run with `sqlite3 … ".read main.sql"` — prefer
the file form so it composes with `files[]` (a `schema.sql` + `seed.sql` + `query.sql`
split, entrypoint `query.sql`). Reuse the exact materialization path
(`materialize.go:113`); add a `sql` `FilePolicy` allowing only `.sql`
(`filepolicy.go:112`), no manifests.

### The genuinely new part: result-set output & determinism

A result set is **tabular**, and for any future exact-match grading the bytes must
be deterministic. Pin the formatting **in `runArgs`**, not left to the caller:

- **Column rendering:** `.mode` fixed (recommend `-json` for a structured result, or
  a pinned `-column`/`-csv` text mode). JSON rows are the cleanest for a backend and
  synergize with the "structured output" direction — the runner emits
  `[{"col":val,...},…]` verbatim, the backend consumes it without parsing text.
- **Ordering:** the runner does **not** impose `ORDER BY` — result order without it
  is engine-defined and the *exercise's* concern; document that the student/backend
  owns ordering. The runner guarantees a stable *rendering* of whatever rows the
  query returns, not a stable *order*.
- **NULL / float / blob rendering:** pinned (`.nullvalue`, JSON encodes NULL as
  `null`; floats via SQLite's default; blobs base64 under the existing
  `Encoding:"base64"` path, `internal/executor/encoding.go`). Document each — this is
  the SQL analogue of G7's locale pin.

### Limits

- **A cartesian `JOIN` is the "infinite loop" of SQL** — the existing per-run
  wall+CPU timeout (`internal/sandbox/nsjail.go:124-125`) already kills it; no new
  control needed.
- **Large result sets / sorts** — the existing output cap
  (`RUNNER_MAX_OUTPUT_BYTES`) bounds the emitted rows; SQLite's temp storage for
  big sorts lands in the size-capped `/tmp` tmpfs (`Spec.TmpfsSizeMB`), so a sort
  bomb hits the tmpfs cap, not host disk.
- Memory: `RLIMIT_AS` + cgroup as any Shape-A language.

## Proposed change — Phase 2: PostgreSQL (deferred; only if PG dialect is needed)

A distinct runtime **shape** because Postgres is client-server. Feasible in the jail
without egress because **`AF_UNIX` works inside the empty netns**:

- **Pre-`initdb` template in the image** (a tiny cluster). Per run: `cp` the template
  into the writable `/tmp` tmpfs (fast — no `initdb` per run, which costs ~0.5–1 s),
  start `postgres` on a **Unix socket** in `/tmp`, run the script via `psql -h /tmp`,
  capture rows, `pg_ctl stop`. The single `execve` is a `/bin/sh -c` prelude
  orchestrating start→query→stop — the exact pattern Go's compile jail already uses
  (`compiled.go:164`, `compileArgv0Absolute`).
- Cost: ~hundreds of ms overhead per run and materially more operational complexity
  (a supervised multi-process lifecycle inside a one-shot jail).
- **Alternative to evaluate: PGlite** (Postgres compiled to WASM, runs in-process via
  a WASM runtime, no server). Gives real PG semantics with no daemon, but is a heavy,
  newer dependency — assess maturity before committing.

Do Phase 2 **only** when a concrete exercise needs PG-specific SQL that SQLite can't
express. Most teaching SQL (SELECT/JOIN/GROUP BY/subqueries/CTEs) runs on SQLite.

## Contract / config impact

- **Phase 1:** one new language id `sql`, a `sql` `FilePolicy` (`.sql` only), and a
  documented result-format contract. Optionally a `result_format` result field
  (`"json-rows"`/`"csv"`) so the backend knows how to read the rows. No sandbox
  change — SQLite is Shape A.
- **Phase 2:** a new runtime shape (socket-orchestrated engine), image growth (the
  PG binaries + template), and a startup/teardown budget — treat like adding a VM
  language (`ADDING-A-LANGUAGE.md:95`).

## Security considerations

*(SQLite adds no new containment surface beyond a normal Shape-A language; the
points to hold:)*

- **No file/network access from SQL.** SQLite can read/write files (`ATTACH`,
  `.import`, `readfile()`), but the run jail is a read-only rootfs with only a
  size-capped `/tmp`; `readfile('/etc/passwd')` returns nothing useful (the jail
  rootfs has no secrets) and the empty netns blocks any network extension. The `.sql`
  file policy plus the jail are the containment; no SQLite-specific extension
  loading (`.load`) — disable it (`sqlite3 -cmd ".dbconfig load_extension off"` or
  a build without extension loading).
- **Determinism is a documented contract**, not an accident (like G7). Pin the
  render mode / NULL / blob encoding in `runArgs`.
- **Phase 2 raises the stakes:** a supervised server process inside the jail is a
  larger surface (the postgres backend runs untrusted SQL). Keep it on the denylist,
  ensure the socket is confined to the per-run `/tmp`, and re-prove egress + host
  survival on-target before shipping.

## Testing

- **Phase 1 success:** `CREATE TABLE … ; INSERT … ; SELECT …` → the expected rows in
  the pinned format; `sql` reported as the language, `runtime_version` = SQLite
  version.
- **Determinism:** the same script twice → byte-identical rows/format; NULL, float,
  and blob rendering match the documented contract.
- **`files[]`:** `schema.sql` + `seed.sql` + `query.sql` with entrypoint `query.sql`
  → correct result (composes with G3).
- **Timeout:** a cartesian join over large synthetic tables → `timeout`.
- **Output cap:** a query returning millions of rows → truncated at the output cap,
  flag set, host healthy.
- **Isolation:** `readfile()`/`.load`/`ATTACH` to anything outside `/tmp` fails
  closed; egress (via any extension) blocked — on-target smoke like a new language.

## Effort

- **Phase 1 (SQLite): S–M.** Mostly a Shape-A spec + the `.sql` file policy + the
  image (`sqlite3`, pinned) + the result-format decision & docs. The format/
  determinism contract is the only non-mechanical part.
- **Phase 2 (PostgreSQL): L.** A new socket-orchestrated runtime shape, image growth,
  and lifecycle/security work. Gate on real demand.

## Phase G10 acceptance

- **Phase 1:** a `sql` submission runs a script against an in-memory SQLite and
  returns rows in a **pinned, documented, deterministic** format; timeout and output
  caps bound a pathological query; `readfile`/extension-load/egress are contained;
  the runner renders no verdict.
- **Phase 2 (if built):** a `sql` (postgres) submission runs against a per-run
  ephemeral cluster over a Unix socket with the same guarantees; the host survives a
  pathological workload; no persistent state survives the run.
