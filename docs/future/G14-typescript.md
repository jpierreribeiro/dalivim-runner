# G14 — TypeScript (compile with `tsc`, run on the Node jail; Shape C mechanically)

TypeScript is not a runtime — it is a **compile step that produces JavaScript**. So
it maps, mechanically, onto **Shape C** (`ADDING-A-LANGUAGE.md:95`): compile to an
intermediate (`tsc` → `.js`), then run it on a "VM" — here the existing Node jail.
It reuses the **generalised compiled runtime** (`internal/executor/compiled.go:264`,
the one Java uses) with `runBin: node`, inheriting the JavaScript run posture whole
(`javascriptSpec`, `internal/executor/languages.go:140`). No new sandbox, no new
shape.

> **The line.** `tsc` **type-checks** and emits `.js`; a type error is a
> `compile_error` (the point of TS), and the emitted `.js` runs in the same
> read-only-rootfs, empty-netns, denylist Node jail the `javascript` language
> already uses. One pinned dependency (`typescript`), baked into the image, offline.

---

## Motivation

TypeScript is the dominant typed-JS teaching language and a real product gap. The
"execute the student's TS" model wants **type errors to fail** (that is what TS is
for), which `tsc` gives for free: it type-checks and refuses to emit on error. And
the run side is already solved — the emitted JS is just JavaScript, so the whole
Node containment (`javascriptSpec`) is reused, not rebuilt.

## Current state

- The generalised compiled runtime (`compiled.go:264`) already supports the "compile
  to an artifact, run it on a launcher" shape via `runBin` + a `{dir}`/`{mem}`-
  templated `run` (`javaSpec`, `compiled.go:197-233`). Java is the landed reference:
  `javac` → `Main.class`, then `java … Main` in a **full-rootfs denylist** run jail
  (`runFullRootfs:true`, `capAddressSpace:false`).
- The `javascript` run posture — `node --disable-proto=throw`, V8 heap bounded by
  `--max-old-space-size` (`nodeHeapArgs`, `languages.go:223`), denylist,
  `capAddressSpace:false` — is defined on `javascriptSpec` (`languages.go:140`) and
  proven on-target.
- The image ships Node 18 (`Dockerfile:65`) but **no `tsc`**; there is no `typescript`
  or `ts` entry in the registries or `filePolicies` (`filepolicy.go:112`).

## Proposed change

### Spec (`compiledLangSpec`)

```go
var typescriptSpec = compiledLangSpec{
    name:       "typescript",
    sourceFile: "main.ts",
    // tsc TYPE-CHECKS and emits JS. Flags are pinned by the runner (never the
    // caller), the determinism analogue of the sql/pytest pins:
    //   --strict            full type-checking (a type error is the point)
    //   --noEmitOnError     emit NOTHING on a type error -> no artifact -> the
    //                       compile phase short-circuits to compile_error
    //   --target ES2020 --module commonjs --lib ES2020   pinned language/runtime
    //   --outDir {dir}      emit main.js into the per-run dir
    // tsc is itself a Node program; run it via node against the bundled compiler so
    // there is no shebang/PATH ambiguity (compileArgv0Absolute).
    compile:  []string{"/usr/local/bin/node", "/opt/typescript/bin/tsc",
                       "--strict", "--noEmitOnError",
                       "--target", "ES2020", "--module", "commonjs", "--lib", "ES2020",
                       "--outDir", "{dir}", "{src}"},
    compileArgv0Absolute: true,
    artifact:  "main.ts.js? -> main.js", // tsc emits main.js from main.ts; validated after compile
    // RUN = the JavaScript posture, verbatim. node runs the emitted .js.
    run:       []string{"--disable-proto=throw", "--max-old-space-size={mem}", "{out}"},
    runBin:    []string{"node"},
    runFullRootfs: true,       // node is dynamically linked (like the JVM) -> full rootfs
    capAddressSpace: false,    // V8 reserves a virtual cage; bound the heap + cgroup
    runEnv:    determinismEnv("PATH=/usr/local/bin:/usr/bin:/bin"),
    versionArgs:  []string{"/opt/typescript/bin/tsc", "--version"}, // "Version 5.6.3"
    parseVersion: secondField, // -> "5.6.3"
}
```

(The exact artifact-name plumbing — `tsc` emits `main.js` from `main.ts` — is a
small detail for `plan()`/artifact validation; the run jail execs `{dir}/main.js`.)

Register in `main.go` `NewService` (`cmd/runner/main.go:80`) via `NewTypeScript(sb,
compiledConfig(cfg))`.

### Image

Bake the compiler, pinned, into the runtime stage:
`npm install -g typescript@5.6.3` (or unpack a pinned tarball to `/opt/typescript`),
world-readable so the jail uid can read it. It is **one** well-known package with no
transitive runtime deps — the same "bundle the tool, pinned" choice G9 made for
pytest / the JUnit jar. No `npm install` ever runs per-request; the run is offline.

### The pinned tsconfig, not the caller's

The compile flags above ARE the config; a caller-supplied `tsconfig.json` is
**forbidden** (file policy). This keeps the type-check strictness and target a
runner guarantee, not a submission knob — the same discipline as the sqlite/pytest
flag pinning.

## Contract / config impact

- One new language id `typescript`; a `typescript` `FilePolicy` (`.ts`; forbid
  `tsconfig.json`, `package.json`, `node_modules`). Multi-file (G3) — sibling
  `import` across `.ts` files — is a follow-up; single-file `main.ts` first.
- No new limit knobs: the compile phase reuses the compiled compile-timeout; the run
  phase reuses the JS memory posture.
- `report_format`/result shape unchanged (ordinary program output).

## Security considerations

- **Run posture is JavaScript's, unchanged**: read-only rootfs, empty netns, denylist
  seccomp, `--disable-proto=throw`, V8 heap + cgroup bound. Nothing new to prove that
  `javascript` hasn't already proven — but egress is re-tested on-target anyway
  (every new runtime re-proves containment, `ADDING-A-LANGUAGE.md:171`).
- **Offline compile**: `tsc` is local; it resolves `--lib` from the bundled compiler,
  never the network. No `npm`/module resolution off the tree (forbidden by the
  policy + the empty netns).
- **Type-check ≠ execution**: a type error stops at `compile_error` with `tsc`'s
  diagnostics; nothing runs (`--noEmitOnError`). This is a feature — the backend can
  grade "does it type-check" distinctly from "does it run."

## Testing

- **Success**: `const x: number = 2 + 2; console.log(x)` → `success` / `"4\n"`.
- **Type error** → `compile_error`, `tsc` diagnostics in `compile_output`, nothing
  executed (`--noEmitOnError`).
- **Runtime error**: `throw new Error("boom")` → `runtime_error` in the Node jail.
- **timeout** (`while(true){}`), **memory** (heap bomb → V8 abort / cgroup OOM),
  **output flood** → same classification as `javascript`.
- **Isolation (on-target)**: egress via `net`/`fetch` fails closed; host write denied.
  Add a `typescript` on-target smoke (a `.ts` that type-checks, runs, and whose
  egress attempt is contained).

## Effort

**M.** The run side is free (JavaScript posture reused). The work is: bundle `tsc`
pinned, wire the compile phase (tsc flags + artifact name), the `.ts` file policy,
and the on-target smoke. No new sandbox, no new memory/seccomp posture.

## Phase G14 acceptance

- A `typescript` submission type-checks with `tsc` and runs the emitted JS in the
  Node jail; a type error is `compile_error`, a runtime throw is `runtime_error`.
- Containment (egress, host-write, memory, timeout) re-proven on-target — identical
  to `javascript`.
- `runtime_version` reports the TypeScript compiler version; the pinned tsconfig is a
  runner guarantee (no caller `tsconfig.json`).
