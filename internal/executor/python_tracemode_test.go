package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// The trace-mode tests drive the Python runtime directly over the netns backend
// (RUNNER_SANDBOX=off, no nsjail needed — the trace hand-back works via cmd.Dir
// like nsjail's writable /sandbox bind), mirroring the test-mode tests. They
// treat the harness as a black box behind the wire contract: run mode=trace, then
// parse the opaque TraceReport and assert the security bounds hold on hostile
// input. python3 must be present (requirePython skips otherwise).

// traceStep mirrors one entry of the harness's steps[] for assertions.
type traceStep struct {
	Step      int    `json:"step"`
	Event     string `json:"event"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Func      string `json:"func"`
	StdoutLen int    `json:"stdout_len"`
	Stack     []struct {
		Func   string `json:"func"`
		File   string `json:"file"`
		Line   int    `json:"line"`
		Locals map[string]struct {
			Repr string `json:"repr"`
			Type string `json:"type"`
		} `json:"locals"`
	} `json:"stack"`
}

// traceReport mirrors the harness's whole document.
type traceReport struct {
	Version   int         `json:"version"`
	Language  string      `json:"language"`
	Steps     []traceStep `json:"steps"`
	StepCount int         `json:"step_count"`
	Truncated struct {
		Steps bool `json:"steps"`
		Bytes bool `json:"bytes"`
	} `json:"truncated"`
	Crash *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		File    string `json:"file"`
		Line    int    `json:"line"`
		Func    string `json:"func"`
	} `json:"crash"`
}

// newPythonTrace builds a Python runtime with explicit trace caps for a test.
func newPythonTrace(t *testing.T, reportBytes, steps int) *interpretedRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewPython(sb, 64*1024, 256, 64, 4_000_000).WithTraceLimits(reportBytes, steps)
}

// traceReq builds a source_code trace request.
func traceReq(src string) runnerapi.RunRequest {
	return runnerapi.RunRequest{Language: "python", Mode: "trace", SourceCode: src, TimeoutMs: 10000, MemoryMB: 256}
}

// runTraceMode runs a request and parses the returned trace document, asserting
// the wire fields are set correctly.
func runTraceMode(t *testing.T, rt *interpretedRuntime, req runnerapi.RunRequest) (runnerapi.RunResult, traceReport) {
	t.Helper()
	res := rt.Run(context.Background(), req)
	if res.TraceFormat != "dalivim-trace-json@1" {
		t.Fatalf("expected trace_format dalivim-trace-json@1, got %q (status=%s stderr=%s)", res.TraceFormat, res.Status, tail(res.Stderr, 400))
	}
	if res.TraceReport == "" {
		t.Fatalf("expected a non-empty trace_report (status=%s stderr=%s)", res.Status, tail(res.Stderr, 400))
	}
	var tr traceReport
	if err := json.Unmarshal([]byte(res.TraceReport), &tr); err != nil {
		t.Fatalf("trace_report is not valid JSON: %v\n%s", err, tail(res.TraceReport, 400))
	}
	if tr.Version != 1 || tr.Language != "python" {
		t.Fatalf("unexpected trace header: version=%d language=%q", tr.Version, tr.Language)
	}
	return res, tr
}

// TestPythonTrace_SuccessfulRun: a clean program traces to success with steps
// over the student file, a captured local value, and the run's normal stdout
// attached ALONGSIDE the trace.
func TestPythonTrace_SuccessfulRun(t *testing.T) {
	requirePython(t)
	res, tr := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"x = 41\nx = x + 1\nprint('value', x)\n"))

	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	if !strings.Contains(res.Stdout, "value 42") {
		t.Fatalf("expected normal stdout alongside trace, got %q", res.Stdout)
	}
	if tr.Crash != nil {
		t.Fatalf("clean run must have no crash, got %+v", *tr.Crash)
	}
	if tr.StepCount == 0 || len(tr.Steps) != tr.StepCount {
		t.Fatalf("expected steps recorded, got step_count=%d len=%d", tr.StepCount, len(tr.Steps))
	}
	for _, s := range tr.Steps {
		if s.File != "main.py" {
			t.Fatalf("only the student file may be traced, saw %q", s.File)
		}
	}
	// The final value of x (42) must appear in some frame's locals.
	if !traceHasLocal(tr, "x", "42") {
		t.Fatalf("expected local x==42 to be captured in the trace")
	}
	// stdout_len must be monotonic non-decreasing and end at the printed length.
	last := 0
	for _, s := range tr.Steps {
		if s.StdoutLen < last {
			t.Fatalf("stdout_len must be monotonic, went %d -> %d", last, s.StdoutLen)
		}
		last = s.StdoutLen
	}
	if last == 0 {
		t.Fatalf("expected cumulative stdout length to advance after print()")
	}
}

// TestPythonTrace_Crash: an uncaught exception classifies runtime_error (exactly
// like run mode) AND records a crash site pointing at the offending student line,
// with the trace still handed back.
func TestPythonTrace_Crash(t *testing.T) {
	requirePython(t)
	res, tr := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"def divide(a, b):\n    return a / b\n\nprint('before')\nprint(divide(10, 0))\n"))

	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("a crashing traced program must be runtime_error, got %s", res.Status)
	}
	if res.ExitCode == 0 {
		t.Fatalf("crashing harness must exit non-zero, got exit 0")
	}
	if tr.Crash == nil {
		t.Fatalf("expected a crash record in the trace")
	}
	if tr.Crash.Type != "ZeroDivisionError" {
		t.Fatalf("expected ZeroDivisionError, got %q", tr.Crash.Type)
	}
	if tr.Crash.Line != 2 || tr.Crash.Func != "divide" || tr.Crash.File != "main.py" {
		t.Fatalf("crash site should be main.py:2 in divide, got %s:%d in %s", tr.Crash.File, tr.Crash.Line, tr.Crash.Func)
	}
}

// TestPythonTrace_StepLimit: a hot loop past the step cap stops recording (the
// harness sets truncated.steps and disables the tracer), so the program still
// completes successfully but the trace is bounded at the cap.
func TestPythonTrace_StepLimit(t *testing.T) {
	requirePython(t)
	res, tr := runTraceMode(t, newPythonTrace(t, 5_000_000, 50), traceReq(
		"t = 0\nfor i in range(100000):\n    t += i\nprint(t)\n"))

	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("step-capped run should still complete, got %s", res.Status)
	}
	if !tr.Truncated.Steps {
		t.Fatalf("expected truncated.steps=true at the step cap")
	}
	if tr.StepCount > 50 {
		t.Fatalf("recorded steps must not exceed the cap 50, got %d", tr.StepCount)
	}
}

// TestPythonTrace_ReportByteCapTruncates: the runner's outer byte cap truncates
// the returned document and sets the authoritative TraceReportTruncated flag,
// independent of the harness's own internal budget.
func TestPythonTrace_ReportByteCapTruncates(t *testing.T) {
	requirePython(t)
	// A cap BELOW the floor of any document. The harness now charges its own
	// envelope against the budget, so at any cap it can meet it writes a valid,
	// under-cap document (see TestPythonTrace_ReportAlwaysFitsAndParses) — which
	// is the point. To still prove the OUTER cap is authoritative and independent
	// of the harness, the cap has to be smaller than the smallest document the
	// harness can produce (measured: 234 bytes with zero steps). This is the
	// defence that matters against a HOSTILE writer: /sandbox is writable in trace
	// mode, so the student's own program can put anything in trace.json.
	res := newPythonTrace(t, 150, 2500).Run(context.Background(), traceReq(
		"vals = list(range(200))\nfor v in vals:\n    pass\nprint('done')\n"))
	if res.TraceReport == "" {
		t.Fatalf("expected a (truncated) trace report")
	}
	if len(res.TraceReport) > 512 {
		t.Fatalf("trace report must be capped at 512 bytes, got %d", len(res.TraceReport))
	}
	if !res.TraceReportTruncated {
		t.Fatalf("expected TraceReportTruncated=true at the outer byte cap")
	}
}

// TestPythonTrace_ReportAlwaysFitsAndParses: o documento que o harness entrega
// tem de CABER no teto e ser JSON VÁLIDO, em qualquer teto.
//
// O orçamento contava só os passos, então ele fechava exatamente em
// MAX_REPORT_BYTES e o documento final — com o envelope (version, limits,
// truncated, o registro de quebra) — passava disso. Aí o teto externo do runner
// cortava o arquivo no meio de um JSON, e o cliente recebia algo que não dá para
// ler: o trace INTEIRO perdido em vez de truncado, e o frontend só podendo
// degradar para "nenhum passo a passo".
//
// É a mesma forma de erro do resto deste trabalho — o limite, ao ser atingido,
// destruía a entrega em vez de degradá-la.
func TestPythonTrace_ReportAlwaysFitsAndParses(t *testing.T) {
	requirePython(t)
	// Um programa que gera muito mais trace do que qualquer um destes tetos.
	const src = "acc = []\nfor i in range(400):\n    acc.append([i, i * 2])\nprint(len(acc))\n"
	for _, cap := range []int{2_000, 20_000, 200_000} {
		res := newPythonTrace(t, cap, 2500).Run(context.Background(), traceReq(src))
		if res.TraceReport == "" {
			t.Fatalf("cap=%d: expected a trace report", cap)
		}
		if len(res.TraceReport) > cap {
			t.Fatalf("cap=%d: harness wrote %d bytes, over its own budget", cap, len(res.TraceReport))
		}
		// O que importa para quem consome: ele PARSEIA. Um documento cortado no
		// meio é indistinguível de nenhum documento.
		var doc map[string]any
		if err := json.Unmarshal([]byte(res.TraceReport), &doc); err != nil {
			t.Fatalf("cap=%d: trace report is not valid JSON (%v): %s", cap, err, tail(res.TraceReport, 120))
		}
		if res.TraceReportTruncated {
			t.Fatalf("cap=%d: the harness stopped in time, so the OUTER cap must not have fired", cap)
		}
	}
}

// --- security: hostile runtime values must never break the harness ----------

// TestPythonTrace_MaliciousReprNeverInvoked: a value whose __repr__ would run
// arbitrary/expensive code (here it prints a marker and raises) must NEVER be
// called by the serializer — the value renders as its class name only, and the
// marker never appears in stdout or the trace.
func TestPythonTrace_MaliciousReprNeverInvoked(t *testing.T) {
	requirePython(t)
	src := "" +
		"class Evil:\n" +
		"    def __repr__(self):\n" +
		"        print('PWNED_MARKER')\n" +
		"        raise RuntimeError('boom in repr')\n" +
		"e = Evil()\n" +
		"y = 1\n" +
		"print('ok')\n"
	res, tr := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(src))

	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("harness must not be destabilized by a hostile __repr__, got %s (stderr=%s)", res.Status, tail(res.Stderr, 300))
	}
	if strings.Contains(res.Stdout, "PWNED_MARKER") || strings.Contains(res.TraceReport, "PWNED_MARKER") {
		t.Fatalf("the value's __repr__ was invoked — it must never be called")
	}
	if !strings.Contains(res.TraceReport, "<Evil>") {
		t.Fatalf("an unknown object must render as its class name <Evil>, trace=%s", tail(res.TraceReport, 300))
	}
	_ = tr
}

// TestPythonTrace_CyclicStructureBounded: a self-referential container must not
// hang the serializer — it is rendered with a <circular> marker and the run
// completes.
func TestPythonTrace_CyclicStructureBounded(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"a = []\na.append(a)\nb = {}\nb['self'] = b\nprint('done')\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("cyclic structures must be handled, got %s", res.Status)
	}
	if !strings.Contains(res.TraceReport, "circular") {
		t.Fatalf("expected a <circular> marker for the self-referential value")
	}
}

// TestPythonTrace_HugeValueBounded: a very long string local must be truncated in
// the trace, never serialized in full (bounded repr size).
func TestPythonTrace_HugeValueBounded(t *testing.T) {
	requirePython(t)
	res, tr := runTraceMode(t, newPythonTrace(t, 5_000_000, 2500), traceReq(
		"big = 'A' * 1000000\nprint(len(big))\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s", res.Status)
	}
	for _, s := range tr.Steps {
		for _, fr := range s.Stack {
			if lv, ok := fr.Locals["big"]; ok {
				if len(lv.Repr) > 1024 {
					t.Fatalf("huge string local must be bounded, got repr len %d", len(lv.Repr))
				}
			}
		}
	}
}

// traceHasLocal reports whether any frame in any step captured a local `name`
// whose rendered repr contains `want`.
func traceHasLocal(tr traceReport, name, want string) bool {
	for _, s := range tr.Steps {
		for _, fr := range s.Stack {
			if lv, ok := fr.Locals[name]; ok && strings.Contains(lv.Repr, want) {
				return true
			}
		}
	}
	return false
}
