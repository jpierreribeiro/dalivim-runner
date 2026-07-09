# G6 — Per-language limit floors

Promoted out of [G5](G5-deferred.md) when its trigger fired: **G2.2 (Java)
landed**, and the JVM's fixed baseline cost makes one global limit policy wrong
for it. Scope is deliberately minimal — **floors only**. Per-language *defaults*
and *ceilings* stay deferred (see the scope note at the bottom).

## Motivation

The request's `memory_mb` becomes **both** the JVM's `-Xmx` heap and the cgroup
`memory.max`, but JVM non-heap overhead (metaspace, code cache, GC structures,
thread stacks) sits **on top of** the heap. A backend that sends a python-sized
budget (say 64 MB) for a Java run gets a VM that dies at startup or spuriously
OOMs — for *every* program, correct or not. That is an executor correctness bug
in student terms ("my hello world got `memory_exceeded`"), not a policy choice.

The same shape of problem can recur on the time axis (a VM whose cold start eats
a short timeout), so the seam covers both timeout and memory floors even though
only Java's memory floor is set today.

## Current state

- Limits are clamped in exactly one place, the service layer:
  `executor.go` `Service.Run` — `≤0` → default, then capped at the global
  ceiling (`Limits`).
- `languageSpec` (`languages.go`) and `compiledLangSpec` (`compiled.go`) carry
  no per-language limit knobs.
- Java (`javaSpec`, `compiled.go`) documents the overhead problem and told the
  backend to "send a generous memory_mb" — a convention, not an enforcement.

## Change (landed with this spec)

Generalise the specs to carry optional floors, applied in the same single
clamping locus:

1. **Spec fields**: `minTimeoutMs` / `minMemoryMB` (0 = no floor) on both
   `languageSpec` and `compiledLangSpec`.
2. **Discovery**: both runtimes expose `LimitFloors() Floors`; the service
   discovers the capability by type assertion (`limitFloorer`), so the
   `Runtime` interface stays minimal and floor-less runtimes are untouched.
3. **Application** (`Service.Run`): after the existing default/ceiling clamp,
   `raiseToFloor` lifts the value to the floor — **still capped at the global
   ceiling**. A floor never overrides operator policy: if a floor exceeds
   `MaxMemoryMB`/`MaxTimeoutMs`, the ceiling wins.
4. **Values**: Java sets `minMemoryMB: 128` (the global default — only
   explicitly undersized requests are lifted). No timeout floor: measured JVM
   cold start is well under a second, comfortably inside the 3 s default.
   No other language sets a floor.

## Contract / config impact

No wire change: no new fields, statuses, or env vars. Observable behaviour
change is narrow and additive-safe: a Java request with `memory_mb < 128` now
*runs with* 128 MB instead of failing at VM startup. The effective limits were
already service-controlled (defaults/ceilings), so callers could never rely on
an exact echo of what they sent.

Floors are compiled-in spec constants, not env-tunable — they encode what the
runtime *needs to start*, which is a property of the image's runtime version,
not a deployment policy. If an operator needs to forbid >N MB runs entirely,
the global ceiling already does that (and wins over any floor).

## Security considerations

- A floor **raises** resource grants, so the thing to protect is the operator
  ceiling: `raiseToFloor` re-caps at the global max after lifting, and the
  misconfiguration edge (floor > ceiling) is pinned by a unit test.
- No new attacker-controlled input: floors come from the closed spec registry,
  never from the request.
- Containment is unchanged — the same cgroup/rlimit machinery enforces the
  (possibly lifted) budget.

## Testing

- `TestService_PerLanguageFloors`: omitted → default raised to floor; below
  floor → raised; above floor → honoured; above ceiling → ceiling wins.
- `TestService_FloorAboveCeilingIsCapped`: floor > ceiling yields the ceiling.
- `TestJavaSpec`: pins `minMemoryMB: 128` on the Java spec.
- Existing clamp/default tests pin that floor-less languages are unaffected.

## Effort

S — landed in the same change as this spec.

## Scope note — what stays deferred

Per-language **defaults** and **ceilings** (a full per-language policy table,
possibly env-tunable) remain YAGNI: one global policy still fits every
registered language once the floor removes the JVM startup hazard. Revisit if a
language needs a *lower* ceiling than the global one (abuse economics) or a
genuinely different default (not just a minimum).
