# Trace mode (`mode:"trace"`) — G16 (Python-Tutor-style step-through)

Trace mode lets the frontend show a **step-through visualizer** of a student's
program — like pythontutor.com. One run returns **both** the normal
`stdout`/`stderr`/`exit_code` of the program **and** a bounded, structured
**execution trace**: for every executed line of the student's own code, a
snapshot of the current line, the call stack, each frame's locals (and the module
globals), and the cumulative length of output produced so far — plus a `crash`
record pointing at the exact line where an uncaught exception was raised.

**The runner stays a dumb, contained executor.** It renders no verdict and draws
no UI. It only *transcribes* what a fixed, in-image trace harness produced, and
hands it back opaque (`trace_report`, labelled by `trace_format`). Stepping
forward/back is done entirely **client-side** over the fetched trace — there is
no re-execution.

> **Status:** ships for **Python** (CPython `sys.settrace`, `dalivim-trace-json@1`)
> and **Odin** (gdb over a `-debug` build, `dalivim-trace-json@2` — see
> [`future/B2-TRACER-SPIKE.md`](./future/B2-TRACER-SPIKE.md) for the security
> review that gated it). The registry is a closed per-language seam
> (`traceCommands`), so others can be added the same way. Ask for support via
> `GET /languages` (`trace:true`).
>
> The two formats differ only in what each mechanism can produce — same document
> shape, same bounds, same delta heap. `@2` replaces v1's `{repr,type}` `locals`
> with the object graph, and carries `prologo` on a frame whose arguments the
> prologue has not made readable yet.

This is run mode **plus a transcription**: the status classifies the *student*
program exactly like run mode (`success` / `runtime_error` / `timeout` /
`memory_exceeded` / `output_limit_exceeded`), and the trace is attached whenever
the harness produced one.

---

## Quick start

```bash
curl -fsS "$RUNNER_URL/run" \
  -H 'content-type: application/json' \
  -H "X-Runner-Token: $RUNNER_SERVICE_TOKEN" \
  -d '{
    "language": "python",
    "mode": "trace",
    "source_code": "def divide(a, b):\n    return a / b\n\nprint(divide(10, 0))\n"
  }'
```

Response (a crashing program — note `status` is `runtime_error` yet the trace is
still handed back, with a `crash` record):

```json
{
  "status": "runtime_error",
  "exit_code": 1,
  "stdout": "",
  "stderr": "",
  "trace_format": "dalivim-trace-json@1",
  "trace_report": "{\"version\":1,\"language\":\"python\",\"steps\":[...],\"crash\":{\"type\":\"ZeroDivisionError\",\"message\":\"'division by zero'\",\"file\":\"main.py\",\"line\":2,\"func\":\"divide\"}}",
  "duration_ms": 24
}
```

`mode:"trace"` accepts either `source_code` (single file, traced as `main.py`) or
`files[]` (a normal multi-file program — validated against the **run** file
policy, entrypoint resolved as in run mode). It honours `stdin` (a traced program
may read input) but **not** a `stdins[]` batch (rejected, 400). Trace runs use the
larger **test-timeout** envelope, since line-by-line tracing is materially slower
than a bare run.

---

## Wire format — `dalivim-trace-json@1`

`trace_report` is a JSON document, opaque to the runner:

```jsonc
{
  "version": 1,
  "language": "python",
  "steps": [
    {
      "step": 11,                 // 0-based index into steps[]
      "event": "line",            // "call" | "line" | "return" | "exception"
      "file": "main.py",          // student file of the innermost frame (rel to trace root)
      "line": 7,                  // 1-based current line in that file
      "func": "main",             // innermost frame's function name ("<module>" at top level)
      "stdout_len": 12,           // cumulative CHARACTERS of stdout produced up to this step
      // "heap" / "heap_delta": ver "O heap viaja em DELTA", abaixo.
      "stack": [                  // OUTERMOST-first; the last entry is the current frame
        { "func": "<module>", "file": "main.py", "line": 13, "locals": { /* module globals, data only */ } },
        { "func": "main",     "file": "main.py", "line": 7,  "locals": {
            "total": { "repr": "1", "type": "int" },
            "i":     { "repr": "2", "type": "int" }
        } }
      ],
      "exc": { "type": "ZeroDivisionError", "message": "'division by zero'" }  // only on "exception" events
    }
  ],
  "step_count": 25,
  // Tetos internos atingidos. `heap` = algum passo bateu no teto de CAIXAS, então
  // o desenho está incompleto: uma referência cujo alvo não coube não alcança
  // nada. Era o único teto sem forma de chegar ao aluno.
  "truncated": { "steps": false, "bytes": false, "heap": false },
  "limits":    { "max_steps": 2500, "max_report_bytes": 2000000 },
  "crash": {                      // present iff the program ended by an uncaught exception
    "type": "ZeroDivisionError",
    "message": "'division by zero'",
    "file": "main.py",
    "line": 2,
    "func": "divide"
  }
}
```

### O heap viaja em DELTA (`heap_encoding: "delta"`)

Repetir o heap inteiro em todo passo era a maior fonte de bytes do documento.
Medido num laço de 120 iterações: **58%** do relatório era heap e **86%** dos
passos repetiam o anterior byte a byte — com o teto de 2 MB, é isso que corta um
trace comum ao meio. O documento traz, no topo, `"heap_encoding": "delta"`, e:

* o **primeiro** passo com grafo carrega `heap` inteiro;
* os seguintes carregam `heap_delta`: `{ "set": { id: box, ... }, "del": [id...] }`;
* um passo **sem nenhum dos dois** tem o heap **idêntico ao anterior**.

Mesmo conteúdo, 61% menos bytes no mesmo programa (321 KB → 124 KB). O consumidor
remonta acumulando — `heapsOf` (Go, nos testes) e `parseTrace` (frontend) fazem
exatamente isso, e o `set` ainda diz **o que** mudou naquele passo, que é o sinal
usado para marcar mutação no desenho.

**Ids de objeto são estáveis pelo trace inteiro.** O mapa `id() → id denso` vive
pelo trace, não pelo passo: antes ele era refeito a cada passo, então um `del a`
renumerava todos os sobreviventes e a identidade — a propriedade que o diagrama
existe para ensinar — não atravessava um passo. Contra reuso de endereço pelo
CPython há duas defesas: o mapa é **podado ao conjunto vivo** de cada passo, e o
**nome do tipo** viaja junto com o id (uma lista liberada cujo endereço vira um
dicionário ganha id novo). Sobra o caso de reuso pelo mesmo tipo entre dois
passos: o efeito é cosmético (a caixa lê como "mudou" em vez de "nasceu") e nunca
um vazamento — o id é um contador denso e nenhum endereço é exposto.

**Values** are rendered as `{ "repr": <bounded string>, "type": <class name> }`.
The `repr` is produced by the harness's own `safe_repr` — see the security model:
it is **not** the value's Python `repr()`.

**Cumulative stdout.** `stdout_len` counts characters (code points) written to
stdout *before* the current line executes. In the compiled shape it comes from
the file the inferior's stdout was redirected into, read incrementally; if the
language's runtime buffers, the number LAGS rather than leads, which is the safe
direction — the consumer slices the captured stdout to it, so a low number shows
a shorter prefix and never output that has not happened yet. The visualizer reconstructs
"output so far at step *i*" by slicing the run's captured `stdout` to that length
(use `Array.from(stdout).slice(0, n).join('')` for code-point-correct slicing).
Program output still flows to the real stdout, so the runner's output-limit kill
and the normal `stdout` field are unaffected.

**Frontend mapping.** Current line → highlight `steps[i].line` in `steps[i].file`
in the editor. Variables panel → `steps[i].stack[*].locals` per frame (the
module frame carries globals). Call-stack panel → `steps[i].stack` (outermost
first). "Crashed here" → the `crash` record's `file`/`line`, reached at the last
step whose `event == "exception"`.

---

## Bounds & limits (all hard, all runner/harness-owned, never request-driven)

| Bound | Default | Knob | Enforced by |
|---|---|---|---|
| Recorded steps | 2500 | `RUNNER_MAX_TRACE_STEPS` | harness (`MAX_STEPS`) → stops recording, sets `truncated.steps` |
| Trace document bytes | 2 MB | `RUNNER_MAX_TRACE_REPORT_BYTES` | harness in-budget stop (`truncated.bytes`) **and** runner outer cap (`trace_report_truncated`) |
| Value repr length | 512 chars/value, 256/scalar | fixed in harness | `safe_repr` (`MAX_REPR`, `MAX_STR`) |
| Container walk | depth 4, 30 items | fixed in harness | `safe_repr` (`MAX_DEPTH`, `MAX_ITEMS`) |
| Vars per frame | 60 | fixed in harness | `_vars_of` (`MAX_VARS`) |
| Stack frames per step | 25 | fixed in harness | `_build_stack` (`MAX_STACK`) |
| Heap boxes per step | 200 | fixed in harness/driver | `MAX_HEAP_OBJECTS` → the step carries `heap_truncated`, the document `truncated.heap` |
| Pointer targets expanded, per step (Odin) | 32 | fixed in driver | `MAX_EXPANDIDOS_POR_PASSO` → past it a pointer stays its address |
| Pointer expansions, whole trace (Odin) | 600 | fixed in driver | `MAX_EXPANSOES_NO_TRACE` — the expansion is steps × boxes, so a per-step cap alone still lets a 150-node loop time out |
| Wall / memory / output | run-mode caps (test-timeout envelope) | existing knobs | the run-mode jail for an INTERPRETED language; see below for Odin |

The harness estimates the document's growth per step and stops **before**
crossing the byte budget; the runner then re-reads the file under an independent
outer cap and sets the **authoritative** `trace_report_truncated` flag (same
discipline as `stdout_truncated` / `test_report_truncated`).

---

## Security model — student code and its runtime values are HOSTILE

The trace harness is `sys.settrace` running over untrusted code. For an
INTERPRETED language it runs in the **exact same jail as run mode** (empty netns,
per-run cgroup `memory.max`, `RLIMIT_NPROC`/`RLIMIT_FSIZE`, read-only rootfs,
`-s -P` CPython hardening, the `LANG`/`LC_ALL`/`TZ`/`PYTHONHASHSEED` determinism
pins). No isolation is relaxed there.

**A COMPILED language is the exception, and it is deliberate.** A native binary
has no `sys.settrace`, so the only way to stop it line by line is a debugger, and
a debugger is `ptrace`. `mode:"trace"` on a compiled language therefore runs in a
SEPARATE jail (`SeccompTracer` + `TracerProcfs`, see `executeTrace`), which is the
run-mode posture plus exactly two things:

* `ptrace`, `process_vm_readv`, `process_vm_writev` — the rest of the denylist
  stays (mount/pivot_root, module and BPF loading, keyrings, reboot/swap, the
  fd-to-path handle tricks, `perf_event_open`, io_uring, userfaultfd);
* a procfs of the **jail's own fresh PID namespace** (4 visible PIDs, read-only),
  without which gdb cannot resolve a PIE load base.

The classic objection — a tracer rewriting the syscall number at the ptrace stop,
after seccomp ran — was TESTED rather than assumed and does not hold on this
kernel: the kernel re-applies the filter to the rewritten call. Everything else
that bounds the blast radius is unchanged: fresh PID namespace, jail-private uid,
read-only rootfs with no setuid binary, `no_new_privs`, empty netns. The full
review is in [`future/B2-TRACER-SPIKE.md`](./future/B2-TRACER-SPIKE.md); the
posture is pinned by `TestTracerPolicy_*` in `internal/sandbox`.

On top of the jail, the harness adds:

1. **`safe_repr` never calls a value's `__repr__`/`__str__`/`__iter__`/`items`.**
   Only built-in scalar/container shapes are walked; **any other object renders as
   `<ClassName>` only**. A malicious, expensive, or side-effecting `__repr__`
   therefore never runs — `TestPythonTrace_MaliciousReprNeverInvoked` (a
   `__repr__` that prints a marker and raises is never invoked).

   **A SUBCLASS OF A BUILT-IN IS STUDENT CODE**, and `isinstance` is true for it.
   Every branch therefore dispatches through the BUILT-IN's own unbound slot
   (`int.__repr__(v)`, never `str(v)`; `dict.items(v)`, never `v.items()`), which
   a subclass cannot override — safe and still truthful, so the value keeps
   showing (a `class Celsius(float)` shows its number, a `Counter` its pairs).
   Measured before this rule, with a `class Loud(int)`: the student's `__str__`
   ran six times without the program calling it once, `Loud(5)` was DRAWN AS 42,
   and a `__str__` returning 200 MB was materialised in full before the length cap
   could cut it. Pinned by `TestPythonTraceGraph_BuiltinSubclassIsNotStudentCode`
   (the student's own counter is the witness) and by
   `TestTraceHarness_BuiltinSlotDispatch` (the rule, not one program).
2. **Cycles and huge structures are bounded** by depth, item-count, and total
   length caps, with an id-visited set that renders a back-reference as
   `<circular>` (`TestPythonTrace_CyclicStructureBounded`,
   `TestPythonTrace_HugeValueBounded`).
3. **Step count is capped**, so an unbounded loop cannot inflate the trace; the
   harness disables the tracer once the cap is hit and the program runs to
   completion (`TestPythonTrace_StepLimit`).
4. **Report bytes are double-capped** (harness budget + runner outer cap).
5. **Only student frames are traced.** Frozen/synthetic code objects
   (`<frozen runpy>`, `<string>`) and stdlib/harness frames are excluded, so the
   trace never leaks harness internals or host paths, and stdlib overhead never
   enters the step budget.
6. **The trace file is read back with the traversal-resistant reader**
   (`readTestReport` — `os.Root` + `Lstat` + `SameFile`), the same hostile-file
   discipline test mode uses: student code shares the writable `/sandbox`, so a
   symlinked `trace.json` pointing at a host secret fails closed.
7. **Recording never destabilizes the program.** Every per-step record is wrapped
   so a serialization failure degrades the trace (a missing/`<unrepr>` value)
   rather than crashing the traced run.

---

## End-to-end flow (runner → backend → engine → frontend)

```
Frontend "Step-through" button
  └─ POST /run { language, mode:"trace", source_code }   (via backend gateway)
       └─ Backend threads mode through, calls RUNNER /run  (existing run plumbing)
            └─ Runner: writes _dalivim_trace.py + main.py into the jail,
               runs  python3 -s -P _dalivim_trace.py  under sys.settrace,
               returns RunResult{ status, stdout, stderr, exit_code,
                                  trace_report, trace_format, trace_report_truncated }
       ◀─ Backend stores/forwards the RunResult unchanged (trace fields passthrough),
          optionally persisting a trace session (ENGINE: execution-trace domain)
  ◀─ Frontend receives BOTH:
        • stdout/stderr/exit  → the existing "Resultado" tab (UNCHANGED)
        • trace_report        → the step-through visualizer in the right (Terminal)
                                 column: variables + call-stack + cumulative-stdout
                                 panels + step forward/back controls (reusing the
                                 ReplayPlayer engine). Current line highlighted in
                                 the CodeEditor. Stepping is pure client-side over
                                 the fetched trace — no re-execution.
```

- **Runner** (this repo): the foundational, self-contained slice — implemented and
  tested here. `internal/executor/trace_harness.py`,
  `internal/executor/tracemode.go`, and the `runTrace`/`executeTrace` path in
  `internal/executor/interpreted.go`; contract fields in
  `pkg/runnerapi/contract.go`.
- **Backend** (`dalivim-backend`): passthrough — accept `mode:"trace"` on the run
  request, forward it to the runner, and relay `trace_report`/`trace_format`/
  `trace_report_truncated` back in the run response using the existing run-result
  plumbing. No grading, no parsing.
- **Engine** (`dalivim-engine`): owns the *execution-trace / step-replay* domain
  concept — a `TraceSession` referencing an `Execution`, storing the opaque trace
  document + format + truncation flag + step count, so a trace can be persisted,
  fetched, and replayed later. Doc-stage: a clean contract over a minimal store.
- **Frontend** (`dalivim-frontend`): the sibling step-through player, reusing the
  existing `ReplayPlayer` timeline engine and the telemetry allowlist. See the
  placement notes above (Terminal column, Resultado tab untouched, highlight in
  the editor).
```
