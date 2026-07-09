# G6 — Batch execution (multi-stdin, compile-once-run-many)

The single biggest latency/cost win for a grading platform, and it stays cleanly
on the **executor** side of the line: the runner runs one program against **many
inputs** and returns **many raw results** — it never compares them to expected
output. The backend still judges.

> **Status — IMPLEMENTED (2026-07-09), demand gate overridden by owner
> decision.** The original grounding stands — the backend has no judge yet, so
> `stdins[]` has no caller today — but the owner chose to build the executor
> side ahead of the judge so it's ready the moment a test-case model lands. The
> contract is additive, so shipping it early costs nothing on the wire. See
> **Implementation notes** at the bottom for the decisions locked at build time.

---

## Motivation
Every real submission runs against a **test suite** of N inputs. Today that is N
separate HTTP calls, each of which sets up a jail, and — for compiled languages —
**recompiles the identical source** N times. Compilation is the expensive step;
paying it once and reusing the artifact against every input collapses the cost of
a submission from `N·(compile + run)` to `compile + N·run`.

This is pure executor work. The runner is handed inputs and returns outputs; it
does not know the inputs are "test cases", does not hold expected outputs, and
does not decide AC/WA. Framing discipline: the field is **`stdins`**, never
"test cases".

## Current state
One request → one runtime → one result. No notion of multiple inputs anywhere.
- `RunRequest` carries a single `Stdin string` (`pkg/runnerapi/contract.go:16-27`);
  `RunResult` is a single struct (`contract.go:30-64`).
- `Service.Run` looks up the runtime and calls `rt.Run(ctx, req)` exactly once,
  clamping limits first (`internal/executor/executor.go:78-96`, clamp at `:101`).
- **Compiled flow** (`internal/executor/compiled.go`): `Run` orchestrates
  compile→execute (`compiled.go:318-339`) — writes source (`:325`), calls
  `compile()` (`:330`), then `execute()` **once** (`:336`). `compile()`
  (`:343-417`) builds the artifact into `workDir` (validated to exist/size at
  `:409-415`). `execute()` (`:422-509`) is a **pure function of `(req, workDir)`**:
  it re-resolves the artifact path each call (`:432-433`), builds a fresh timeout
  ctx (`:423`), fresh output buffers (`:473-474`), sets `cmd.Env = r.spec.runEnv`
  (`:468-470`), binds `cmd.Stdin = strings.NewReader(req.Stdin)` (`:464`), and
  `cmd.Run()`s a new nsjail invocation (`:479`).
- **Interpreted flow** (`internal/executor/interpreted.go`): single run in `Run`
  (`:68-163`); stdin bound at `:117`; `cmd.Run()` at `:129`.

**SEAM:** because the compiled artifact persists in `workDir` after `compile()`
returns and `execute()` is idempotent w.r.t. it, `Run` can **loop `execute()` over
N stdins against the one already-built artifact with zero change to the compile
phase.** That is the whole feature.

## Proposed change

### Contract
Add an optional `stdins` array; keep single `stdin` as sugar. Exactly one is set.

```go
type RunRequest struct {
    // ... existing: Language, SourceCode, TimeoutMs, MemoryMB, CompileTimeoutMs ...
    Stdin  string   `json:"stdin,omitempty"`  // single-run sugar (unchanged)
    Stdins []string `json:"stdins,omitempty"` // batch: one execution per element
}
```

Response: when `stdins` is set, return a batch envelope — the **shared** compile
telemetry once, plus one `RunResult` per input, **index-aligned**:

```go
type BatchResult struct {
    RuntimeName    string       `json:"runtime_name"`
    RuntimeVersion string       `json:"runtime_version"`
    CompileMs      int64        `json:"compile_ms,omitempty"`
    CompileOutput  string       `json:"compile_output,omitempty"`
    Status         string       `json:"status"`            // compile_error, or "ok" if compiled
    Results        []RunResult  `json:"results"`           // per input; empty on compile_error
    Aborted        bool         `json:"aborted,omitempty"` // batch-total budget hit; Results is partial
}
```

When `stdins` is **absent**, the response is the existing single `RunResult`,
**byte-for-byte unchanged**.

### Execution flow
- **Compiled**: `compile()` once (unchanged), then loop `execute()` over each
  stdin against the same `workDir` artifact. Each iteration gets its own fresh
  timeout ctx, output buffers, cgroup, and nsjail invocation — no state carries
  between inputs. **This is where the win concentrates** (compile amortized).
- **Interpreted**: no artifact to reuse — write source once, then loop-launch the
  interpreter per stdin. Saves the HTTP round-trips and repeated source
  write/validation, not the per-input process launch. Do it uniformly for API
  symmetry; be honest that the payoff is smaller than for compiled languages.
- **Sequential, not parallel-within-batch.** Parallel inputs would fight the
  global concurrency limiter and muddy per-run cgroup accounting. Keep it
  sequential — the compile is already amortized, which was the point.

### Resource accounting (the crux — this is a DoS control)
A batch of N inputs × per-run timeout is up to N× the work of a single call while
holding **one** concurrency slot. Bound it explicitly:
- **`RUNNER_MAX_BATCH`** — cap the number of inputs (e.g. 100); reject larger with
  `400`.
- **`RUNNER_MAX_BATCH_TOTAL_MS`** — a batch-wide wall budget. When cumulative time
  crosses it, stop and mark the batch `Aborted: true` with a partial `Results`
  (the backend re-queues the rest). Prevents one request from monopolizing a
  worker for `N × max_timeout`.
- The concurrency limiter counts a batch as **one** in-flight slot for its whole
  duration — so true worst-case wall pressure is
  `RUNNER_MAX_CONCURRENT_RUNS × RUNNER_MAX_BATCH × per_run_timeout`. Size the caps
  against that product and document it.
- Per-input memory/pids/cgroup limits are unchanged (each execution is fully
  independent).
- Per-input stdin cap (`RUNNER_MAX_STDIN_BYTES`, G1) applies to each element; add
  a **total** stdin-bytes cap across the batch.

### Semantics
- **Compile failure** → the whole batch is `compile_error`, returned once (no
  artifact, no per-input results).
- **Per-input failures** (`runtime_error`/`timeout`/`memory_exceeded`) are
  **independent** — one bad input never aborts the others; the backend wants every
  result. Only the batch-total budget aborts early (`Aborted`).
- **Index-aligned, deterministic order** — `results[i]` corresponds to `stdins[i]`.

## Contract / config impact
- Additive `stdins[]` request field + batch response envelope; single-stdin path
  unchanged.
- New env: `RUNNER_MAX_BATCH`, `RUNNER_MAX_BATCH_TOTAL_MS`, batch total-stdin cap.
- No new *run* status needed — a response-level `Aborted` flag is simpler than a
  per-input `batch_aborted` status. (Decide at implementation; flag preferred.)
- **No judge semantics.** The envelope carries raw stdout/status per input;
  comparison to expected stays entirely in the backend.

## Security considerations
- The **batch-total budget is the containment control** — without it a single
  request is a DoS. Treat it with the same seriousness as the concurrency limiter.
- **Fresh jail per input is mandatory.** The reused artifact must be exec'd in a
  new nsjail invocation each iteration (it is — `execute()` builds one per call,
  `compiled.go:422+`). A reused/long-lived jail across inputs would be a
  cross-input isolation bug — high severity. Assert no state (files in `/tmp`,
  leftover processes) survives between inputs.
- Total stdin cap bounds memory pressure from a batch of large inputs.

## Testing
- **Regression**: no `stdins` → response is byte-for-byte the current single
  `RunResult`.
- **Compile-once proof**: batch of N against a compiled language → exactly **one**
  compile (instrument the compile call count / assert `compile_ms` appears once,
  not N times).
- **Per-input independence**: batch where input 3 times out and the rest succeed →
  `results[3].status == timeout`, all others `success`.
- **Batch-total budget**: a batch that exceeds `RUNNER_MAX_BATCH_TOTAL_MS` →
  `Aborted: true`, partial results, host healthy.
- **Isolation**: input A attempts a write / spawns a process; input B sees none of
  it.
- **On-target latency**: one compiled submission vs 20 stdins → wall ≈
  `compile + 20·run`, not `20·(compile+run)`.

## Effort
**M.** The contract is additive and the compiled seam is genuinely clean (loop
`execute()`); the real design work is the resource-accounting caps, not the loop.

## Phase G6 acceptance
- A compiled batch compiles **once** and runs N times; wall ≈ `compile + N·run`.
- Single-stdin behaviour is byte-for-byte unchanged.
- The batch-total budget bounds worst-case work per request; the host survives a
  max-size, max-timeout batch.
- The runner returns N **raw** results; no comparison or verdict anywhere.

## Implementation notes (as landed)

- **Envelope**: `runnerapi.BatchResult` as specced; the "decide at
  implementation" choice went to the response-level `aborted` flag (no new
  per-run status). Batch-level `status` is `"ok"` (`BatchStatusOK`),
  `compile_error`, or `internal_error` (the last maps to 503 + `Retry-After`,
  same failover contract as a single run). Provenance is stamped once on the
  envelope; per-input results carry no `runtime_*`/`compile_ms`.
- **Dispatch**: `Service.RunBatch` mirrors `Run` (same normalize → clamp →
  floors pipeline, shared via `clampLimits`); runtimes implement the optional
  `batchRunner` interface, discovered by assertion like `limitFloorer`.
  `Service.Run` rejects a `stdins[]` request so batches can't slip down the
  single path. The shared sequential loop + budget lives in `batchLoop`
  (`batch.go`).
- **Budget epoch includes the compile**: a compile that eats the whole
  `RUNNER_MAX_BATCH_TOTAL_MS` aborts before the first input. The budget is
  checked *between* runs (each run is already bounded by its own timeout), so
  the worst overshoot is one per-run timeout past the budget.
- **Env**: `RUNNER_MAX_BATCH` (100), `RUNNER_MAX_BATCH_TOTAL_MS` (60000),
  `RUNNER_MAX_BATCH_STDIN_BYTES` (4000000). Count/bytes are enforced at the
  transport next to the existing stdin cap (each element also obeys
  `RUNNER_MAX_STDIN_BYTES`); the wall budget is the executor's.
- **Isolation caveat (on-target concern)**: inputs share the per-batch
  `workDir` (that's the amortization). Under **nsjail** it is mounted read-only
  in every run jail and `/tmp` is a fresh per-jail tmpfs, so no state survives
  between inputs — assert this in the on-target smoke (input A writes, input B
  must not see it). The `off`/netns dev fallback cannot make that guarantee
  (writable cwd), consistent with its generally weaker posture.
- **Tests**: `batch_test.go` (compile-once proven by a jail-counting fake
  sandbox; index alignment; per-input independence; budget abort end-to-end;
  validation) and `handlers_test.go` (envelope, caps, stdin/stdins
  exclusivity). Single-run regression: the whole pre-existing suite.
- **Multi-file works too**: `files[]` + `stdins[]` compose — the batch path
  reuses the same plan/prepare seams G3 introduced.
