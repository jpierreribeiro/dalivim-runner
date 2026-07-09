# G3 — Multi-file submissions

> **Status: implemented.** The runner side is done — `files[]`/`entrypoint`
> contract, the traversal-resistant materialization layer, and multi-file
> build/run for C/C++/Java/Go/Python/JS, with the adversarial path/symlink/limit
> test suite green. See [`../G3_MULTIFILE.md`](../G3_MULTIFILE.md) for the design,
> threat model, security checklist, and residual-risk report. **Backend
> coordination (producing `files[]` from the student editor) is the remaining
> half** and is tracked there.

The largest phase and the only one that **changes the wire contract**. Enables a
student to submit several files — own headers (`#include "util.h"`), multiple
translation units, a package split — instead of one blob. Schedule deliberately;
it touches the runner contract, the compile flow, **and the backend**.

---

## Motivation
Real coursework past week 1 is multi-file: a `.h`/`.c` pair, a Java class per
file, a Go package. Today every submission is a single fixed-name file, so those
tasks are impossible. This is the difference between "runs toy snippets" and
"runs real assignments".

## Current state
Source is always one file, one fixed basename, from `req.SourceCode`:
- write `main.py`/`main.js`/`main.c`/`main.cpp` (`interpreted.go:76`,
  `compiled.go:125`); the compile/run argv reference that single name.
- `RunRequest` has `source_code string` only (`contract.go:15-26`) — no files
  array, no entrypoint, no filenames.

## Proposed change

### Contract
Add an optional `files` array; keep `source_code` as single-file sugar:

```go
type RunFile struct {
    Path    string `json:"path"`    // relative, e.g. "util.h", "src/main.c"
    Content string `json:"content"`
}
type RunRequest struct {
    Language   string    `json:"language"`
    SourceCode string    `json:"source_code,omitempty"` // sugar: == files:[{path:<default>, content}]
    Files      []RunFile `json:"files,omitempty"`
    Entrypoint string    `json:"entrypoint,omitempty"`  // which file is main; default per language
    Stdin      string    `json:"stdin,omitempty"`
    // ... existing limits ...
}
```

Exactly one of `source_code` / `files` is set. `source_code` maps to a single
file at the language's default entry name (`main.c`, `Main.java`, …). With
`files`, `entrypoint` names the main file (default: the language's convention).

### Filesystem materialisation
Write every file under the jail workdir (`/sandbox`), creating parent dirs. This
is the **security-critical** step — see below.

### Compile flow per language
- **C/C++**: compile *all* `.c`/`.cpp` translation units together:
  `gcc -O2 -static -o {out} {all .c files} {link}`. Headers (`.h`) are just
  written to disk and found via `-I{workdir}` (add `-I.`). The `{src}` template
  in `compiledLangSpec` (G1) generalises from one file to "all source-extension
  files in the tree".
- **Go**: `go build` a directory/module. Write a `go.mod` (or synthesise one) and
  `go build -o {out} ./...` — Go's own build system handles multi-file. Multi-file
  Go is actually *simpler* than C here.
- **Java**: `javac -d {dir} {all .java files}`; run `java -cp {dir} {Entrypoint
  class}`. Multiple classes/files are the norm.
- **Python/JS**: write all files; run the interpreter on the entrypoint
  (`python3 -I {entry}`, `node {entry}`). Imports/`require` of sibling files just
  work because they're on disk.

### Entrypoint resolution
Default per language: `main.c`/`main.cpp`/`main.py`/`main.js`/`Main.java`/`main.go`.
Overridable via `Entrypoint`. Validate it exists in `files`.

## Security considerations (the crux)
Writing caller-controlled paths is a **path-traversal / arbitrary-write** risk.
Enforce, in the handler before touching disk:
- **Reject absolute paths** and any component `..` (no escaping the workdir).
- **Reject symlink-y / special** names; allow a conservative charset
  (`[A-Za-z0-9._/-]`), forbid leading `/`, forbid empty components.
- **Normalise then re-check**: `filepath.Clean` and confirm the result still has
  the workdir as prefix (defence in depth against clever encodings).
- **Caps**: max file count (e.g. 50), max per-file bytes, max **total** bytes
  (extend `RUNNER_MAX_SOURCE_BYTES` → a `RUNNER_MAX_FILES_BYTES` sum), max path
  depth/length.
- Materialisation happens **inside** the jail workdir which is already a
  size-capped tmpfs and thrown away after the run — but the write itself is done
  by the runner process *before* the jail execs, so the traversal check is the
  only thing standing between a malicious `path` and the host FS. Treat it as the
  highest-severity validation in the codebase.

## Contract / config impact
- New `files[]`, `entrypoint` fields (additive; `source_code` still works).
- New env: `RUNNER_MAX_FILES`, `RUNNER_MAX_FILES_BYTES` (or reuse/extend source
  cap).
- New `400` validation errors (bad path, too many files, oversized).
- **Backend coordination**: the backend must send `files[]` for multi-file tasks
  and map them from the student's editor/upload. This is a joint change — the
  runner accepting `files[]` is half; the backend producing them is the other.

## Testing
- Unit (security): a table of malicious paths (`../etc/passwd`, `/etc/passwd`,
  `a/../../b`, `x/./../../y`, empty, 4 KB path, symlink names) all → `400`, no
  write outside workdir. This is the most important test in the phase.
- Unit: `source_code` sugar still writes the single default file.
- On-target per language: a `.h`+`.c` pair (C), two `.java` files with a helper
  class, a Go package with two files, a Python program importing a sibling module
  — all `success`.
- Regression: every single-file test still passes unchanged.

## Effort
L — contract change, per-language compile changes, a rigorous path-safety layer,
and backend coordination. Do **after** G1/G2 so the per-language compile specs
(and Go/Java) already exist to generalise from.

## Phase G3 acceptance
- Multi-file C/C++/Java/Go/Python/JS submissions compile and run.
- The path-traversal test table is exhaustive and green; no write ever escapes
  the workdir.
- Single-file `source_code` path is byte-for-byte unchanged in behaviour.
- Backend sends `files[]` for the tasks that need it.
