# G3 — Multi-file submissions

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

Both runtime kinds need it: `interpreted.go` and `compiled.go` each write a
*single* file today. Factor **one shared `materialize(workDir, files)` helper** in
the executor package that both call — don't duplicate the write+validation logic.
Split the checks by layer: the **shape validation** (path charset, `..`, count/size
caps → `400`) lives in the handler *before* touching disk; the **`filepath.Clean`
prefix re-check** lives inside the shared writer as the last line of defence.

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
- **Go is a shell-injection surface, uniquely.** The other compiled langs `exec`
  the compiler directly (argv, no shell), but the Go compile is a
  `/bin/sh -c "..."` prelude (it seeds the pre-warmed `GOCACHE`;
  `compileArgv0Absolute: true`). Expanding `{srcs}` into that shell string injects
  caller-derived paths into a shell command. The charset allowlist
  (`[A-Za-z0-9._/-]`) already blocks shell metacharacters, but **shell-quote each
  path anyway** as defence in depth — this is the subtlest interaction in the
  phase, and the one spot where the traversal charset and the compile builder meet.

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
- **`smoke-run.sh` can't send `files[]`** — it takes a single `source_code`
  (+ `SMOKE_STDIN`, added in G1). Add a `files[]`-capable helper for the on-target
  multi-file CI steps: extend it with a `SMOKE_FILES` JSON env, or add
  `scripts/smoke-files.sh`.
- **CI base-rate gotchas** (don't chase these as G3 bugs): the CI
  `runner-smoke` container runs `cgroup=auto` (rlimit-only, **no delegated
  cgroup**) → don't assert Go/Java `memory_exceeded` there, it needs the cgroup;
  the openjdk/Go toolchains make the image build heavy (fine, just slow); the
  `trivy` job flakes on GitHub-API rate limits (already hardened with retry+token
  on main); and a tight interpreter timeout under `-race` flakes (Node tests sit
  at 8 s for this reason).

## Implementation notes (field-tested from G1/G2/G4)
G1, G2, and G4 have landed, so G3's preconditions are met — the seams to
generalise from now exist, and G3 is **more mechanical than "L" reads**:

- **`{src}` → `{srcs}` is a token→list expansion**, not another replacer pair.
  Today `{src}` is substituted 1:1 via `strings.NewReplacer` in `subst()`. Multi-
  file needs one `{srcs}` token that expands into N paths — a small structural
  change to the compile-argv builder. Filter by extension per language: C/C++
  compile only `.c`/`.cpp` translation units (headers just sit on disk, found via
  the `-I.` you must add); Java compiles all `.java`; Go builds all `.go`.
- **Java's entrypoint is a class name, not a path.** The run argv hardcodes `Main`
  today. For `Entrypoint`, derive the class from the filename (`Foo.java` → `Foo`),
  keep `Main`/`Main.java` as the default, and update the artifact-existence check
  (it validates `Main.class` today) to validate the entrypoint's `.class`. The
  public-class-==-filename rule still applies per file.
- **Interpreted entrypoint**: only the entrypoint file is executed; siblings just
  need to be on disk for `import`/`require`. The `runArgs` reference `sourceFile`
  today — make that the *resolved entrypoint*.

## Effort
L in raw scope (contract change, per-language compile changes, a rigorous
path-safety layer, backend coordination) — but the seams above make it more
mechanical than that grade suggests. Do **after** G1/G2 (done) so the per-language
compile specs and Go/Java already exist to generalise from.

## Phase G3 acceptance
- Multi-file C/C++/Java/Go/Python/JS submissions compile and run.
- The path-traversal test table is exhaustive and green; no write ever escapes
  the workdir.
- Single-file `source_code` path is byte-for-byte unchanged in behaviour.
- Backend sends `files[]` for the tasks that need it.
