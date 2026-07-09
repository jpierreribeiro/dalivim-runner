# G3 — Multi-file submissions: design, threat model, and risk report

Status: **implemented** (this document is the deliverable for the G3 phase; the
planning spec is [`future/G3-multi-file-submissions.md`](future/G3-multi-file-submissions.md)).

G3 lets a submission span several files — own headers (`#include "util.h"`),
multiple translation units, a Java package, a Go package, a Python program
importing sibling modules — instead of a single fixed-name blob. It is the only
phase that changes the wire contract, and its file-materialization step is the
highest-severity code in the repo: the request path is attacker-controlled and the
write happens on the host before the jail execs. The governing principle:

> **The host must survive any malicious submission. Correctness is secondary to
> containment. If a feature creates ambiguity, reject it.**

---

## 1. Wire contract

`files[]` and `entrypoint` are added; `source_code` stays as single-file sugar and
is **unchanged, byte-for-byte**.

```go
type RunFile struct {
    Path    string `json:"path"`    // relative, forward-slash, validated
    Content string `json:"content"`
}
type RunRequest struct {
    Language   string    `json:"language"`
    SourceCode string    `json:"source_code,omitempty"` // single-file (legacy, unchanged)
    Files      []RunFile `json:"files,omitempty"`       // multi-file (new)
    Entrypoint string    `json:"entrypoint,omitempty"`  // main file / Java class; default per language
    Stdin      string    `json:"stdin,omitempty"`
    // ... existing limits ...
}
```

Rules (all violations → HTTP `400`, never a run outcome):

1. Exactly one of `source_code` / `files` is set (both/neither → `400`).
2. `source_code` maps to the language's default entry file, exactly as before.
3. `files[]` has 1…`RUNNER_MAX_FILES` entries; total ≤ `RUNNER_MAX_FILES_BYTES`;
   each ≤ `RUNNER_MAX_FILE_BYTES`.
4. `entrypoint` defaults per language; path-based for Python/JS/C/C++/Go (must
   exist in `files`), a class name for Java (validated as a class, not a path).
5. Duplicate normalized paths → `400`. Disallowed extension / forbidden manifest /
   forbidden directory → `400`.

Entry defaults: `main.c`, `main.cpp`, `main.py`, `main.js`, `main.go`, and `Main`
(Java).

### Allowed extensions & forbidden names (start restrictive)

| Lang | Allowed | Compiled | Forbidden by name / component |
|---|---|---|---|
| C | `.c .h` | `.c` | (extension allowlist rejects Makefile, `.so`, …) |
| C++ | `.cpp .cc .cxx .hpp .hh .h` | `.cpp .cc .cxx` | — |
| Java | `.java` | `.java` | — |
| Go | `.go` | `.go` | `go.mod`, `go.sum`, `go.work`, `vendor/` |
| Python | `.py` | — | `setup.py`, `pyproject.toml`, `conftest.py`, `__pycache__/` |
| JS | `.js .mjs .cjs .json` | — | `package.json`, `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `node_modules/` |

> Deviation from the spec's literal grammar `^[A-Za-z0-9][A-Za-z0-9._/-]*$`:
> **underscore is allowed** (added to the charset, and as a leading component
> char). Real submissions are full of it — snake_case filenames and Python dunder
> files (`__init__.py`, `__main__.py`) — and `_` carries no path or shell meaning,
> so admitting it is safe and necessary. Every other restriction is unchanged.

---

## 2. Threat model

The attacker controls `language`, every `path`, every `content`, `entrypoint`, and
`stdin`. They cannot control the runner's flags, limits, env, or the jail. The
assets to protect, in priority order:

1. **Host filesystem integrity** — no write, symlink-follow, or parent-dir
   creation outside the per-job root. (Highest severity: the write is on the host.)
2. **Host secrets / credentials** — the worker holds none (Law 2); nothing in a
   job can read host config, caches, sockets, or metadata endpoints.
3. **Network** — denied by default (empty netns, no lo, no DNS). No package
   download, no egress from code, compiler, or build system.
4. **Host availability** — bounded output, wall/CPU time, memory, pids, file size,
   and file count/size; a compile is itself attacker input and runs jailed.

Attack surfaces and the control that closes each:

| Surface | Attack | Control |
|---|---|---|
| `path` | `../`, absolute, symlink, backslash, NUL, unicode bidi, deep/long | Path grammar + per-component rules → `400`; `openat2`/`os.Root` writer |
| `path` | pre-existing hostile object (symlink/hardlink/FIFO/dev) race | `O_CREATE|O_EXCL`, `MkdirAll` under `os.Root`, post-write `lstat` |
| `path` | filename read as a compiler flag (`-Wl,…`) or argfile (`@f`) | grammar forbids leading `-`/`@`; argv is absolute in-jail paths, no shell |
| `content` | compile bomb (macro/template/recursion) | compile runs in a jail with cgroup/rlimit/seccomp + compile timeout + output cap |
| manifest | `go.mod`/`package.json`/`setup.py` to trigger a build system | forbidden by name; no package manager is ever invoked |
| Go build | fetch an external module | synthesized module + `GOPROXY=off GOSUMDB=off GOWORK=off`, empty netns |
| entrypoint | Java class name treated as a path; path traversal via entrypoint | class-name regex for Java; path entrypoint validated + existence-checked |
| output | unbounded stdout/stderr/compile diagnostics | per-stream caps, kill-on-flood (`output_limit_exceeded`) |

---

## 3. Security checklist

### File materialization (`internal/executor/materialize.go`, `filepolicy.go`)

- [x] Validate file count, per-file bytes, total bytes **before** any write.
- [x] Whole-path grammar rejects absolute, backslash, control/NUL, non-ASCII,
      leading `-`/`@`, leading `.`.
- [x] Per-component rules reject `.`, `..`, empty components, dotfiles anywhere,
      forbidden components (`node_modules`, `vendor`, …).
- [x] Path length ≤ `RUNNER_MAX_PATH_BYTES`, depth ≤ `RUNNER_MAX_PATH_DEPTH`.
- [x] `path.Clean` is a no-op (reject anything not already normal form).
- [x] Reject duplicate normalized paths.
- [x] Extension allowlist per language; forbidden-manifest names rejected.
- [x] Write with a traversal-resistant API: `os.Root` (Go 1.24+, `openat2` +
      `RESOLVE_BENEATH`, no symlink following) — **never** `filepath.Clean` +
      `strings.HasPrefix` alone.
- [x] Parent dirs created by the runner under the root (`os.Root.MkdirAll`), never
      adopted from a supplied symlink.
- [x] Files created `O_CREATE|O_EXCL|O_WRONLY` (never follow/clobber).
- [x] Post-write `lstat`: every object is a regular file owned by the runner uid.
- [x] Re-validate inside `MaterializeFiles` (defense in depth over the transport's
      fail-fast check) — the function is safe to call on its own.
- [x] No archive upload (zip/tar/jar) — explicitly out of scope this phase.

### Build & run (`internal/executor/compiled_multifile.go`, `languages.go`)

- [x] Untrusted code is never compiled on the host unsandboxed — build runs in the
      nsjail compile jail, execution in a separate run jail.
- [x] Every compiler/runtime invocation is an argv array (no `sh -c` on user
      input); the one shell prelude (Go's cache seed) interpolates only a
      grammar-restricted, shell-quoted package dir.
- [x] Absolute in-jail paths for all argv file references; sources under
      `/sandbox/src`.
- [x] C/C++: all TUs compiled together, `-I/sandbox/src` for headers, `-static`.
- [x] Go: build the entrypoint's package dir only (never `./...`), synthesized
      `go.mod`, offline (`GOPROXY=off …`), per-job GOCACHE.
- [x] Java: entrypoint is a class name; `javac` to `/sandbox/classes`, run
      `java -cp /sandbox/classes <class>`.
- [x] Python: sibling imports via a controlled `runpy` wrapper with the source
      root (a hardcoded, non-user path) on `sys.path` — not an accidental cwd
      entry; keeps `-I` isolation.
- [x] JS: `node -- <entry>` (`--` stops flag parsing); no npm, no lifecycle
      scripts, no user `NODE_OPTIONS`.

### nsjail (unchanged from F-B/F-D — inherited by the multi-file path)

- [x] Fresh mount/pid/ipc/uts/user/net/cgroup namespaces per run; no external
      interface, no loopback; `no_new_privs`; seccomp (denylist, or static
      allowlist for C/C++ run jail).
- [x] rlimits: `AS`/`CPU`/`FSIZE`/`NOFILE`/`NPROC`/`STACK`/`CORE=0`; cgroup
      memory/pids caps; wall-time.
- [x] Read-only rootfs; size-capped `nodev,nosuid` tmpfs `/tmp`; `--disable_proc`;
      no host `/home`, no Docker socket, no cloud metadata, no SSH agent, no host
      package cache.

---

## 4. Where it lives

| Concern | File |
|---|---|
| Wire types | `pkg/runnerapi/contract.go` |
| Caps (config) | `internal/config/config.go` |
| Per-language policy + validation (→ 400) | `internal/executor/filepolicy.go` |
| Traversal-resistant writer | `internal/executor/materialize.go` |
| Request normalization / dispatch | `internal/executor/executor.go` (`normalize`) |
| Interpreted multi-file argv (runpy / node) | `internal/executor/languages.go` |
| Compiled multi-file build/run recipes | `internal/executor/compiled_multifile.go` |
| Single-vs-multi bifurcation | `interpreted.go` (`prepare`), `compiled.go` (`plan`) |
| HTTP `400` mapping, body cap | `internal/transport/httpapi/handlers.go` |

The single-file path is deliberately **untouched**: both runtimes branch on
`len(req.Files) == 0` and take the exact original code for `source_code`, so the
legacy behavior is byte-for-byte identical (regression tests confirm).

---

## 5. Testing

- **Path-traversal table** (`materialize_test.go: TestValidatePath_TraversalTable`)
  — every hostile path from the spec (`../etc/passwd`, `/etc/passwd`, `a/../../b`,
  `.`, `x//y`, NUL, `\n`, backslash, `-evil.c`, `@args.c`, `.git/config`,
  non-ASCII, bidi override, over-depth, over-length) is rejected. This is the most
  important test in the phase.
- **Symlink / race** (`TestMaterializeFiles_RefusesHostileTree`) — a pre-existing
  symlinked parent, a symlink at the final path, a FIFO, and a hardlink all make
  materialization fail closed with no write escaping the root.
- **Duplicates, limits, extensions, forbidden names, entrypoints** — dedicated
  table tests.
- **Per-language builder tests** (`multifile_test.go`) — the compile/run argv for
  C, C++, Go, Java plus the interpreted tails, as pure functions.
- **Execution** (`multifile_exec_test.go`) — Python sibling import, package import,
  custom entrypoint, and JS sibling require run end-to-end through the service
  under the netns backend (no nsjail needed). Compiled languages run in the
  on-target `runner-smoke` CI job via `scripts/smoke-files.sh` (nsjail required).
- **HTTP** (`handlers_test.go`) — multi-file success `200`; both/neither,
  traversal, absolute, forbidden, missing-entrypoint all `400`.
- **Regression** — every existing single-file test passes unchanged.

---

## 6. Backend coordination (this is only half the feature)

**The runner accepting `files[]` does not make multi-file end-to-end usable.** The
backend must produce `files[]` from the student's editor/upload and:

1. Store submissions as single- or multi-file, preserving relative paths.
2. Enforce the same broad limits before calling the runner.
3. Send `files[]` for multi-file tasks; keep `source_code` for legacy single-file.
4. Render compiler errors with paths relative to the submitted tree.
5. Prevent hidden teacher/test files from being smuggled into student `files[]`.
6. Never let students override language, flags, limits, or build commands.
7. Version the contract if older backend workers may still exist (the additive
   `files[]`/`entrypoint` fields keep old single-file callers working untouched).

Until the backend sends `files[]`, this phase is dormant but non-breaking.

---

## 7. Residual risk / out of scope (deliberate)

Not implemented this phase, by design:

- **Archive upload** (zip/tar/gzip/jar). Adds Zip-Slip, decompression bombs, and
  file-count/symlink handling — a separate phase reusing the same materialization
  API, with its own tests.
- **Dependencies / package managers** (pip, npm/yarn/pnpm, Maven/Gradle, Make,
  CMake, Go modules from a proxy). Any future support must be a controlled feature
  over a read-only internal mirror, never network at build time.
- **User-provided `go.mod` / build files.** Rejected for now; a synthesized module
  is always used.
- **Separate `src`/`build`/`out` bind mounts (Law 6 topology).** The current jail
  uses one per-job bind (`/sandbox`, read-write only during compile) plus a
  size-capped tmpfs `/tmp`; source lives under `/sandbox/src`, artifacts alongside.
  Host integrity is fully preserved (ro rootfs, empty netns, seccomp, cgroup,
  `no_new_privs`); making `/sandbox/src` read-only *during its own build* and
  splitting the writable output into a distinct mount is a defense-in-depth
  refinement, not a containment gap. Deferred.
- **Static seccomp allowlist for the compiled run jail** remains opt-in
  (`RUNNER_STATIC_SECCOMP`) and unchanged by G3.

Known non-security limitations:

- Non-ASCII filenames are rejected (conservative); UTF-8 path support is a future
  opt-in with confusable/bidi handling.
- Go/Java `memory_exceeded` classification depends on the cgroup dial
  (`RUNNER_CGROUP`); the on-target smoke container runs rlimit-only, so those
  verdicts are validated on the VPS, not in the smoke job.
