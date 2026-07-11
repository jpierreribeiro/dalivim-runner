# SQL execution (`language: "sql"`) — G10

The runner can execute SQL: given a script (DDL + DML + `SELECT`), it runs it
against an **ephemeral, in-memory SQLite** database and returns the result set as
**JSON rows**. Like every other language it stays a **dumb, contained executor** —
it runs the SQL and returns the rows **raw**; it never compares result sets, never
grades, never persists anything.

> **Engine:** phase 1 is **SQLite** (`sqlite3 :memory:`) — serverless, in-process,
> zero startup. The wire id is `sql`; the engine is an implementation detail.
> PostgreSQL (a socket-orchestrated shape) is a deliberate future addition, only if
> a concrete exercise needs PG-specific SQL SQLite can't express.

---

## Quick start

```bash
curl -fsS "$RUNNER_URL/run" \
  -H 'content-type: application/json' \
  -H "X-Runner-Token: $RUNNER_SERVICE_TOKEN" \
  -d '{
    "language": "sql",
    "source_code": "CREATE TABLE emp(id INT, dept TEXT, sal INT);\nINSERT INTO emp VALUES (1,'\''eng'\'',100),(2,'\''eng'\'',120),(3,'\''sales'\'',90);\nSELECT dept, COUNT(*) n, AVG(sal) avg_sal FROM emp GROUP BY dept ORDER BY dept;"
  }'
```

Response:

```json
{
  "status": "success",
  "result_format": "json-rows",
  "exit_code": 0,
  "stdout": "[{\"dept\":\"eng\",\"n\":2,\"avg_sal\":110.0},\n{\"dept\":\"sales\",\"n\":1,\"avg_sal\":90.0}]\n",
  "runtime_name": "sql",
  "runtime_version": "3.45.1"
}
```

The result set is the JSON array in `stdout`. `result_format: "json-rows"` tells the
backend how to read it.

---

## The contract

- **Input:** `source_code` is the whole SQL script (a single string). `sql` is
  **source_code-only** in phase 1 — a `files[]` request is rejected with `400`.
  (Multi-file SQL is a documented follow-up: SQLite `.read` is cwd-relative, so a
  clean composition model is deferred rather than shipped half-formed.)
- **Output:** each `SELECT` emits a **JSON array of row objects** on `stdout`, in
  the order the engine returns rows. `result_format` is `"json-rows"`.
- **Status:**
  - `success` — the script ran to completion (`exit_code` 0).
  - `runtime_error` — a SQL error (bad table/column, constraint, syntax). The run
    is **fail-fast** (`-bail`): the first error halts the script with a non-zero
    exit and the engine's message on `stderr`.
  - `timeout` — a runaway query (a cartesian blow-up or an unbounded recursive CTE
    is the "infinite loop" of SQL) hit the wall/CPU budget.
  - `output_limit_exceeded` / `memory_exceeded` — a giant result set or a memory
    bomb hit the output / memory cap, same as any language.

## Determinism (a pinned contract, like G7's locale)

The output format is **pinned by the runner**, not left to the caller, so the same
script yields the same bytes every run:

- **Rendering:** JSON mode (`-json`), fixed. Numbers and text render canonically;
  **`NULL` → JSON `null`**; REAL renders via SQLite's default (e.g. `7.0`).
- **Row order:** the runner does **not** inject `ORDER BY`. Result order *without*
  `ORDER BY` is engine-defined and is the **exercise's** concern — a query that
  needs a stable order must say `ORDER BY`. The runner guarantees a stable
  *rendering* of whatever rows the query returns, not a stable *order*.
- **BLOBs:** rendered by SQLite's `-json` default (a `\u`-escaped string). For
  binary-safe columns use `hex(col)` in the query, or set `encoding: "base64"` to
  base64 the whole `stdout` stream.

## Limits

No new controls — SQL reuses the standard ones:

- **Timeout** bounds a cartesian join / runaway CTE (`timeout_ms`, clamped to the
  service ceiling).
- **Output cap** (`RUNNER_MAX_OUTPUT_BYTES`) bounds a huge result set; the
  `stdout_truncated` flag is authoritative.
- **Memory** is bounded by `RLIMIT_AS` + the cgroup, like any Shape-A language.

---

## Security & containment

SQLite adds **no new containment surface** beyond a normal interpreted language —
it runs in the **identical jail** as python / javascript / lua (read-only host
rootfs, size-capped `/tmp` tmpfs, empty network namespace, seccomp denylist, per-run
cgroup + rlimits). So the egress / host-write / secret-read guarantees are the same
ones the escape corpus (`scripts/smoke-escape.sh`) proves for those languages.

SQL-specific points:

- **No network.** SQL has no socket primitive without an extension, and the empty
  netns blocks egress structurally.
- **Extension loading is DISABLED.** The `sqlite3` CLI enables `load_extension()`
  by default; the runner turns it off (`.dbconfig load_extension off`, applied with
  its stdout echo suppressed so the JSON stays clean). This closes the one primitive
  that could pull in native code. `load_extension(...)` is refused → `runtime_error`.
- **File functions are contained by the jail, not disabled.** SQLite's
  `readfile()` / `writefile()` / `ATTACH` exist, but the rootfs is read-only and
  holds no secrets, `/tmp` is a per-run tmpfs discarded after the run, and there is
  no egress — so `readfile('/etc/passwd')` returns nothing useful and a write cannot
  escape or persist. On-target smokes assert the SQL-specific pieces (pinned format,
  error classification, extension-load disabled, timeout bound).

---

## Notes

- `batch` (`stdins[]`) is accepted for `sql` but has no effect — a SQL script
  defines its own data and ignores stdin, so each batch element re-runs the same
  script. Prefer a single request.
- The image ships `sqlite3` pinned (see the `Dockerfile`).
