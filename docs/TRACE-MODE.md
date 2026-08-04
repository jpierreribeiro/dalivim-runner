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

> **Status:** ships **Python only** (CPython `sys.settrace`). The registry is a
> closed per-language seam (`traceCommands`), so JS/others can be added later
> exactly like test mode. Ask for support via `GET /languages` (`trace:true`).

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
  "truncated": { "steps": false, "bytes": false },  // harness-internal caps hit
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
stdout *before* the current line executes. The visualizer reconstructs
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
| Wall / memory / output | run-mode caps (test-timeout envelope) | existing knobs | the **same jail** as run mode |

The harness estimates the document's growth per step and stops **before**
crossing the byte budget; the runner then re-reads the file under an independent
outer cap and sets the **authoritative** `trace_report_truncated` flag (same
discipline as `stdout_truncated` / `test_report_truncated`).

---

## Security model — student code and its runtime values are HOSTILE

The trace harness is `sys.settrace` running over untrusted code. It runs in the
**exact same jail as run mode** (empty netns, per-run cgroup `memory.max`,
`RLIMIT_NPROC`/`RLIMIT_FSIZE`, read-only rootfs, `-s -P` CPython hardening, the
`LANG`/`LC_ALL`/`TZ`/`PYTHONHASHSEED` determinism pins). No isolation is relaxed.
On top of the jail, the harness adds:

1. **`safe_repr` never calls a value's `__repr__`/`__str__`.** Only built-in
   scalar/container shapes are walked; **any other object renders as
   `<ClassName>` only**. A malicious, expensive, or side-effecting `__repr__`
   therefore never runs — this is proven by `TestPythonTrace_MaliciousReprNeverInvoked`
   (a `__repr__` that prints a marker and raises is never invoked).
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
