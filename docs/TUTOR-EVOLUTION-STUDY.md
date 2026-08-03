# Study — Evolving the step-through tutor: a faithful Python-Tutor visual, and Odin support

Status: **research / design only**. No code in this document ships behavior; it
scopes two pieces of work the team flagged as worth studying, grounds them in the
runner and frontend as they exist today, and gives each a phased plan with honest
effort and risk. Nothing here starts until it is turned into its own tracked task.

Companion to [`TRACE-MODE.md`](./TRACE-MODE.md) (the shipped Python step-through)
and the compiled-language registry in
[`internal/executor/compiled.go`](../internal/executor/compiled.go).

---

## 0. Where we are

The shipped step-through (`mode:"trace"`, Python only) returns a bounded,
per-line trace: for each step, the current file/line, the call stack, each
frame's locals/globals as **`{name, type, repr}`**, cumulative stdout, and a
crash marker. The frontend steps over those snapshots client-side and shows a
call-stack list, a variables table, stdout, and a current-line highlight.

That delivers the *pedagogy of stepping* but not the *picture* Python Tutor is
known for, and it is Python-only. This study covers the two asks that follow from
that:

- **Part A** — make the visual faithful to Python Tutor: heap **objects as
  boxes**, **pointers** drawn from variables to those objects, aliasing and
  cycles shown as shared boxes, collections drawn as indexed cells.
- **Part B** — bring in **Odin**: first just *running* it, then the much harder
  question of a *step-through tutor* for a compiled, debug-info language.

A short **Part C** ties them together: the trace wire format should become
language-agnostic once a second language and a richer visual both arrive, so we
do not grow one dialect per language.

---

## Part A — A faithful Python-Tutor object graph

### A.1 What Python Tutor actually draws

Python Tutor's frame is not a variables *table*; it is a **two-column reference
diagram**:

- **Left — Frames.** The stack, outermost (Global) first. Each frame lists its
  variable names. A variable holding a primitive (`int`, `bool`, `None`, small
  `str`) shows the value inline. A variable holding anything else shows a **stub
  with an arrow** leaving the frame.
- **Right — Objects (the heap).** Every non-primitive is a **box** drawn once:
  a `list` as a row of indexed cells, a `dict` as key→value rows, a function as a
  labelled block, an instance as a field table. Arrows from frame variables (and
  from inside other objects) point at these boxes.

Three properties make it *teach*, and they are exactly what a flat
`{name,type,repr}` table cannot express:

1. **Identity & aliasing.** Two variables bound to the *same* list point at *one*
   box. `repr` alone makes them look like two equal-but-separate values.
2. **Mutation over time.** Because the box has identity, appending to a list
   visibly grows *that* box across steps.
3. **Cycles & nesting.** `a.append(a)` is a box with an arrow back into itself —
   only representable in a graph, never in a string.

### A.2 The gap, precisely

The current trace throws identity away at the source: `safe_repr` renders each
value to a bounded string and stops. To draw the diagram we must instead emit the
**object graph reachable from the frames**, with stable ids, and let primitives
stay inline.

### A.3 Runner change — trace format v2 (`dalivim-trace-json@2`)

Evolve the per-step payload from "variables as reprs" to "variables as
references into a per-step (or per-trace) heap table":

```jsonc
{
  "step": 42,
  "event": "line",
  "line": 11,
  "stack": [
    { "func": "<module>", "vars": { "seq": {"ref": 140} } },
    { "func": "generate", "vars": {
        "n":   {"prim": "int",  "text": "10"},
        "seq": {"ref": 140},          // SAME id → same box as the module's seq
        "i":   {"prim": "int",  "text": "9"}
    } }
  ],
  "heap": {
    "140": { "kind": "list",  "items": [ {"prim":"int","text":"0"}, {"prim":"int","text":"1"}, {"ref": 141} ] },
    "141": { "kind": "list",  "items": [ ... ] },              // nested / shared
    "150": { "kind": "object","cls": "Node", "fields": { "val": {"prim":"int","text":"3"}, "next": {"ref":150} } } // cycle
  }
}
```

Design points:

- **Value = primitive or reference.** `{"prim","text"}` renders inline;
  `{"ref": id}` points at a heap box. A closed set of `kind`s the frontend knows
  how to draw: `list`, `tuple`, `set`, `dict`, `object`, `function`, `module`,
  and a fallback `opaque` (the current `<ClassName>` behavior) for anything we
  refuse to expand.
- **Ids are `id(obj)`**, emitted as strings and stable *within a trace*, so the
  frontend can keep a box in place across steps and animate mutation. They are
  **not** leaked as raw addresses in any security-relevant way — they are opaque
  labels; we can even remap to a per-trace counter to avoid exposing ASLR
  addresses (a tiny hardening win, recommended).
- **Heap: per-trace vs per-step.** Per-step is simplest to consume but
  quadratic in size (the whole reachable heap re-serialized every line).
  Recommended: a **per-trace heap with per-step diffs** — emit a box the first
  time it is seen and, on later steps, only the boxes whose contents changed
  (keyed by id). This keeps the "watch the list grow" animation while bounding
  size. If diffing proves fiddly, ship per-step first behind the existing byte
  cap and optimize later.

### A.4 Serialization algorithm (harness side)

Replace the string-only `safe_repr` walk with a **bounded BFS over reachable
objects**:

1. Roots = the locals/globals of every frame on the stack.
2. For each value: if primitive (or a short `str`/`bytes` under the inline cap),
   inline it; else assign/lookup its id, and if unseen, enqueue its children
   (list items, dict entries, `__dict__` fields, closure cells).
3. **Every existing bound stays and gains a graph-shaped sibling:**
   - `MAX_STEPS` unchanged.
   - `MAX_HEAP_OBJECTS` per step (new) — stop expanding, mark remainder
     `opaque`/truncated.
   - `MAX_ITEMS` per collection (already the item cap) — render first N cells +
     an ellipsis cell.
   - depth cap → beyond it, children are `opaque` refs, not expanded.
   - **Cycles are free**: BFS with a seen-set by id already terminates on
     `a.append(a)`; the cycle simply becomes an arrow. This is strictly *simpler*
     than the current recursive repr, which had to guard cycles defensively.
4. **The hostile-`__repr__` guarantee is preserved and, in fact, stronger:** we
   read structure via `type`, `list`/`dict` iteration, and `vars()` — never the
   object's own `__repr__`/`__str__` for container expansion. Only leaf primitives
   and the `opaque` fallback produce text, and the fallback uses the class name,
   not a user method. Re-run the existing malicious-`__repr__`, cyclic, and
   huge-structure tests against v2.

### A.5 Frontend — the object canvas

New component (sibling of `TraceStepPlayer`, or a mode inside it):

- **Two columns**: Frames (reuse the current stack list, but a variable that is a
  `ref` renders a small source anchor) and Objects (the heap boxes).
- **Box renderers**, one per `kind`: list/tuple/set → indexed cells; dict → k/v
  rows; object → class header + field rows; function/module → labelled block.
- **Arrows**: SVG overlay. Each `ref` anchor and each box registers its DOM rect
  (a `ResizeObserver` + a layout pass); draw a cubic Bézier from anchor→box.
  Aliases = two anchors into one box; cycles = a box whose field anchor points
  back to itself. A lightweight layout (objects stacked in first-seen order,
  arrows routed with simple horizontal offsets) is enough to start — no graph
  auto-layout engine needed for the sizes we cap to.
- **Theme-aware from day one** using the tokens the current player was just
  migrated to (`--uq-paper*`, `--uq-ink`, `--uq-indigo`, `--uq-border`, …), so it
  works in light / dark / dracula / Alto contraste. This is the piece the current
  visual was criticized for; the canvas must not repeat it.
- **Everything already client-side**: stepping still walks fetched snapshots; the
  only new runtime cost is arrow layout on step change.

### A.6 Effort & phasing (Part A)

| Phase | Scope | Rough size |
|---|---|---|
| A0 | Trace v2 schema + harness graph serialization (per-step heap), tests | M |
| A1 | Frontend object canvas: boxes + arrows for list/dict/object, aliasing | L |
| A2 | Cycles, sets/tuples, functions/closures, ellipsis/opaque states | M |
| A3 | Per-trace heap + per-step diff (size/perf), mutation animation | M |
| A4 | Fullscreen layout, polish, a11y (arrows need text-equivalents) | S–M |

Ship A0+A1 as the first visible milestone (real boxes and arrows for the common
cases); A2–A4 harden and complete it. Backend/engine passthrough is unchanged —
the trace stays an opaque blob to them; only its internal schema version bumps.

---

## Part B — Odin

Odin (<https://odin-lang.org>) is a compiled systems language: `odin build file
-out:bin` then run the binary, or `odin run`. It emits native code and **DWARF
debug info** by default in debug builds. That last fact is the whole story for
the tutor.

Split the ask cleanly, because the two halves are worlds apart in cost.

### B.1 Running Odin (tractable — a registry entry)

Odin is "just another compiled language" to the runner. It slots into the
existing `compiledLangSpec` registry alongside C/Go/Rust
([`compiled.go`](../internal/executor/compiled.go)), which already models a
compile phase, an artifact hand-back, and a separate compile timeout:

- `name: "odin"`, `sourceFile: "main.odin"`, `binNames: ["odin"]` (for version
  provenance), compile command `odin build {dir} -out:{out}` (package-directory
  model — Odin compiles a *directory*, which actually fits the multi-file `files[]`
  shape nicely), run the produced binary under the same jail as every other
  compiled artifact.
- Determinism pins (LANG/LC_ALL/TZ) and the sandbox are inherited unchanged; no
  new syscall surface — Odin's binary is as untrusted as a C binary, and the
  nsjail/netns containment already covers it.
- Base image needs the Odin toolchain (a Dockerfile addition; Odin ships LLVM as
  a dependency, so mind image size — measure before committing).
- Cost: **small–medium**, mostly Dockerfile + one registry entry + tests, the
  same recipe C#/Java followed. No contract change: `mode:"run"`/`"test"` work as-is.

This is worth doing on its own merits (a new language for the platform)
independent of any tutor.

### B.2 A step-through tutor for Odin (the hard part)

There is **no `sys.settrace` for a native binary.** The interpreter hook that
made Python cheap does not exist. Realistic options, worst-to-best for us:

1. **Compiler/source instrumentation** — rewrite the Odin source to log line and
   variable state. Rejected: needs a real Odin parser, changes the program under
   test, and cannot see values without type info anyway.
2. **Emulation / DBI** (e.g. a Valgrind-style tool) — far too heavy, and still
   needs DWARF to name variables.
3. **DWARF + a debugger driver (recommended path).** Odin's debug build already
   carries DWARF line tables and variable/location info. Drive a debugger
   non-interactively:
   - `gdb --batch` with a Machine-Interface (`gdb/mi`) or Python-scripting
     session, or `lldb` via its Python API;
   - single-step by source line (`-exec-step`/`next`), and at each stop read the
     line (DWARF line table) and the in-scope locals (DWARF DIEs → memory reads →
     typed values);
   - serialize each stop into the **same trace v2 format** as Part A. A pointer
     in Odin becomes a `ref`; a struct becomes a `kind:"object"`; a slice/array
     becomes indexed cells. The object-graph model from Part A is language-neutral
     enough to receive this.

**Why it is genuinely hard (be honest with stakeholders):**

- **Sandboxing a debugger.** `ptrace` (what gdb/lldb use) inside the nsjail/netns
  jail is a real security review, not a config flag: ptrace is a privilege
  surface, and we currently run untrusted code with *no* such tooling attached.
  This needs its own threat model and probably a separate, more-restricted trace
  jail profile. **This is the tall pole, not the DWARF reading.**
- **Value reconstruction.** Turning DWARF + raw memory into `{kind, fields,
  items}` is per-type plumbing (pointers, slices `{data,len}`, maps, unions,
  strings as `{data,len}`). Doable, tedious, and Odin-version-sensitive.
- **Bounds & hostility.** A native program can scribble memory; the reader must
  treat every pointer as untrusted (bad/dangling pointers → `opaque`, never a
  crash of the tracer), and keep the step/време caps. Optimized builds destroy
  locals, so the tutor requires a **debug build** (`-debug`), which we control.
- **Determinism.** Stepping a native binary is more timing-sensitive than a
  bytecode tracer; keep it off the hot path and single-threaded programs only, at
  least initially.

**Recommendation for B.2:** treat it as a *research spike*, not a feature commit.
Prove the smallest end-to-end slice — `gdb --batch` stepping a trivial `-debug`
Odin binary **inside the sandbox**, emitting three or four trace-v2 steps — and
let that spike's findings (especially the ptrace-in-jail security review) decide
whether it graduates. Do **not** promise a date before the spike.

---

## Part C — Make the trace language-agnostic (do this once, when the 2nd consumer lands)

The moment either "trace v2 object graph" (Part A) or "Odin steps" (Part B.2)
exists, the trace stops being "the Python thing." Lock in one contract so we do
not fork:

- The **wire format** (`dalivim-trace-json@2`) is language-neutral: frames, a
  heap of typed boxes, primitives-vs-refs. Python's `sys.settrace` harness and a
  hypothetical Odin/gdb driver are just two **producers** of the same schema.
- The runner keeps its "**closed per-language trace registry**" discipline (like
  `traceCommands` today): each language names its own producer; the format is
  shared; the backend/engine keep treating the blob as opaque, keyed by
  `trace_format`.
- The **frontend object canvas** consumes the schema, not a language — so Odin
  gets the Python-Tutor visual for free once its producer emits v2.

This is the payoff that makes Part A worth doing *before* Part B rather than
bolting a second bespoke visual on later.

---

## Recommended sequencing

1. **Part A (faithful visual), phases A0–A1** — highest user-visible value, all
   in tech we already run (Python harness + React), no new security surface.
2. **Part C** — fold the v2 schema into a language-neutral contract as A0 lands
   (cheap if done then, expensive if retrofitted).
3. **Part B.1 (run Odin)** — independently valuable, small, ships whenever.
4. **Part B.2 (Odin tutor)** — a gated research spike; the ptrace-in-sandbox
   review is the go/no-go, and it reuses Part A's object graph and Part C's
   contract, so it is cheapest *after* those exist.

## Risk register

| Risk | Part | Mitigation |
|---|---|---|
| Heap serialization blows the size budget | A | Per-trace heap + per-step diffs; hard `MAX_HEAP_OBJECTS`; ellipsis/opaque |
| Arrow layout jank at large graphs | A | We cap object count; simple first-seen stacking; no auto-layout engine |
| Exposing raw `id()`/addresses | A | Remap ids to a per-trace counter |
| Odin toolchain bloats the image | B.1 | Measure; consider a separate builder stage / on-demand image |
| ptrace inside the jail widens attack surface | B.2 | Separate hardened trace profile; dedicated threat model; spike-gated |
| DWARF value reconstruction is Odin-version-fragile | B.2 | Pin toolchain; `opaque` fallback for anything unrecognized |
| Optimized builds hide locals | B.2 | Force `-debug`; document the limitation |

## Open questions

- Per-trace heap with diffs, or per-step full heap first and optimize later?
- Cap policy for the object graph: total boxes vs bytes vs both (both, likely).
- Odin: package-directory compile maps to `files[]` — confirm the entrypoint
  story for a single `main.odin`.
- B.2: gdb/MI vs lldb-python inside the sandbox — which is easier to contain?
