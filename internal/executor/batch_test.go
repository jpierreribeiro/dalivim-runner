package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// batchStub is a stubRuntime that also implements batchRunner, recording the
// clamped request and budget the service handed it.
type batchStub struct {
	stubRuntime
	gotBudget int
	res       runnerapi.BatchResult
}

func (s *batchStub) RunBatch(_ context.Context, req runnerapi.RunRequest, totalBudgetMs int) runnerapi.BatchResult {
	s.got = req
	s.gotBudget = totalBudgetMs
	return s.res
}

// TestService_RunRejectsStdins pins that a stdins[] request cannot slip through
// the single-run path — the transport must route it to RunBatch.
func TestService_RunRejectsStdins(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Stdins: []string{"a"},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError for stdins on Run, got %v", err)
	}
}

// TestService_RunBatchValidation pins the batch shape rules: at least one
// input, stdin/stdins mutually exclusive, the language must support batch, and
// unknown languages fail the same way as single runs.
func TestService_RunBatchValidation(t *testing.T) {
	stub := &batchStub{stubRuntime: stubRuntime{lang: "python"}}
	svc := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512, MaxBatchTotalMs: 60000}, stub)

	var ve *ValidationError
	if _, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{Language: "python", SourceCode: "x"}); !errors.As(err, &ve) {
		t.Fatalf("empty stdins: expected ValidationError, got %v", err)
	}
	if _, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Stdin: "a", Stdins: []string{"b"},
	}); !errors.As(err, &ve) {
		t.Fatalf("stdin+stdins: expected ValidationError, got %v", err)
	}
	if _, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "ruby", SourceCode: "x", Stdins: []string{"a"},
	}); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("unknown language: expected ErrUnsupportedLanguage, got %v", err)
	}

	// A runtime without batchRunner is rejected as a validation error, not a crash.
	plain := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512}, &stubRuntime{lang: "python"})
	if _, err := plain.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Stdins: []string{"a"},
	}); !errors.As(err, &ve) {
		t.Fatalf("non-batch runtime: expected ValidationError, got %v", err)
	}
}

// TestService_RunBatchClampsAndStamps pins that the batch path shares the exact
// limit policy of the single path (defaults, ceilings, floors) and stamps
// provenance on the envelope.
func TestService_RunBatchClampsAndStamps(t *testing.T) {
	stub := &batchStub{
		stubRuntime: stubRuntime{lang: "java"},
		res:         runnerapi.BatchResult{Status: runnerapi.BatchStatusOK, Results: []runnerapi.RunResult{}},
	}
	svc := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512, MaxBatchTotalMs: 45000}, stub)

	res, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "java", SourceCode: "x", Stdins: []string{"a", "b"}, TimeoutMs: 999999, MemoryMB: 999999,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 10000 || stub.got.MemoryMB != 512 {
		t.Fatalf("batch limits not clamped: %+v", stub.got)
	}
	if stub.gotBudget != 45000 {
		t.Fatalf("batch budget not passed: got %d", stub.gotBudget)
	}
	if res.RuntimeName != "java" || res.RuntimeVersion != "9.9.9" {
		t.Fatalf("provenance not stamped on envelope: %+v", res)
	}
}

// TestBatchLoop pins the loop mechanics without any runtime: index alignment,
// the batch-total budget abort (partial prefix), and the ctx-cancel abort.
func TestBatchLoop(t *testing.T) {
	echo := func(_ context.Context, stdin string) runnerapi.RunResult {
		return runnerapi.RunResult{Status: runnerapi.StatusSuccess, Stdout: stdin}
	}

	results, aborted := batchLoop(context.Background(), []string{"a", "b", "c"}, time.Now(), 60000, echo)
	if aborted || len(results) != 3 || results[0].Stdout != "a" || results[2].Stdout != "c" {
		t.Fatalf("index alignment broken: aborted=%v results=%+v", aborted, results)
	}

	// Budget already exhausted (epoch in the past): abort before the first input.
	results, aborted = batchLoop(context.Background(), []string{"a", "b"}, time.Now().Add(-time.Second), 100, echo)
	if !aborted || len(results) != 0 {
		t.Fatalf("exhausted budget must abort with partial results: aborted=%v n=%d", aborted, len(results))
	}

	// Budget crossed mid-batch: the loop stops between inputs, keeping the prefix.
	slow := func(_ context.Context, stdin string) runnerapi.RunResult {
		time.Sleep(30 * time.Millisecond)
		return runnerapi.RunResult{Status: runnerapi.StatusSuccess, Stdout: stdin}
	}
	results, aborted = batchLoop(context.Background(), []string{"a", "b", "c", "d", "e"}, time.Now(), 40, slow)
	if !aborted || len(results) == 0 || len(results) >= 5 {
		t.Fatalf("mid-batch budget must abort with a partial prefix: aborted=%v n=%d", aborted, len(results))
	}

	// A cancelled caller stops the loop cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, aborted = batchLoop(ctx, []string{"a"}, time.Now(), 0, echo)
	if !aborted || len(results) != 0 {
		t.Fatalf("cancelled ctx must abort: aborted=%v n=%d", aborted, len(results))
	}

	// 0 disables the budget entirely.
	results, aborted = batchLoop(context.Background(), []string{"a", "b"}, time.Now().Add(-time.Hour), 0, echo)
	if aborted || len(results) != 2 {
		t.Fatalf("budget 0 must run everything: aborted=%v n=%d", aborted, len(results))
	}
}

// batchService wires real interpreted runtimes (isolation off) so batch tests
// drive the full service path exactly as the transport does.
func batchService(t *testing.T, maxBatchTotalMs int) *Service {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewService(
		Limits{
			DefaultTimeout: 8000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512,
			MaxBatchTotalMs: maxBatchTotalMs,
		},
		NewPython(sb, 64*1024, 256, 64),
	)
}

// TestPython_BatchIndexAligned is the interpreted batch acceptance: one source,
// N stdins, N index-aligned raw results on a BatchStatusOK envelope.
func TestPython_BatchIndexAligned(t *testing.T) {
	requirePython(t)
	res, err := batchService(t, 60000).RunBatch(context.Background(), runnerapi.RunRequest{
		Language:   "python",
		SourceCode: "import sys\nprint('got:' + sys.stdin.read().strip())\n",
		Stdins:     []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if res.Status != runnerapi.BatchStatusOK || res.Aborted {
		t.Fatalf("batch envelope wrong: %+v", res)
	}
	if len(res.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(res.Results))
	}
	for i, want := range []string{"got:a\n", "got:b\n", "got:c\n"} {
		if res.Results[i].Status != runnerapi.StatusSuccess || res.Results[i].Stdout != want {
			t.Fatalf("results[%d]: status=%q stdout=%q want %q", i, res.Results[i].Status, res.Results[i].Stdout, want)
		}
	}
}

// TestPython_BatchPerInputIndependence pins that one failing input never
// affects the others — the backend wants every raw result.
func TestPython_BatchPerInputIndependence(t *testing.T) {
	requirePython(t)
	res, err := batchService(t, 60000).RunBatch(context.Background(), runnerapi.RunRequest{
		Language:   "python",
		SourceCode: "import sys\ns = sys.stdin.read().strip()\nif s == 'boom':\n    sys.exit(3)\nprint(s)\n",
		Stdins:     []string{"ok1", "boom", "ok2"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	want := []string{runnerapi.StatusSuccess, runnerapi.StatusRuntimeError, runnerapi.StatusSuccess}
	for i, w := range want {
		if res.Results[i].Status != w {
			t.Fatalf("results[%d].status = %q, want %q (independence broken): %+v", i, res.Results[i].Status, w, res.Results)
		}
	}
	if res.Results[1].ExitCode != 3 {
		t.Fatalf("failing input's exit code lost: %+v", res.Results[1])
	}
}

// TestPython_BatchTotalBudgetAborts pins the DoS control end-to-end: a batch
// whose inputs outlast the batch-wide wall budget stops early with Aborted and
// a partial prefix, instead of holding the slot for N × timeout.
func TestPython_BatchTotalBudgetAborts(t *testing.T) {
	requirePython(t)
	res, err := batchService(t, 700).RunBatch(context.Background(), runnerapi.RunRequest{
		Language:   "python",
		SourceCode: "import time\ntime.sleep(0.4)\nprint('done')\n",
		Stdins:     []string{"a", "b", "c", "d", "e", "f"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !res.Aborted {
		t.Fatalf("expected aborted batch, got %+v", res)
	}
	if n := len(res.Results); n == 0 || n >= 6 {
		t.Fatalf("expected a partial prefix, got %d results", n)
	}
}

// countingSandbox is a fake containment backend that counts jail invocations:
// a Writable spec is the compile jail (it fabricates the artifact the runtime
// validates), a read-only spec is the run jail (`cat`, so stdout == stdin and
// index alignment is provable). It makes "compile once, run N times" an exact
// assertion instead of a wall-clock heuristic — and needs no real toolchain,
// since the netns test backend cannot mount /sandbox for real compile argv.
type countingSandbox struct {
	compiles    int
	runs        int
	failCompile bool
}

func (s *countingSandbox) Command(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, sandbox.RunAccounting) {
	if spec.Writable { // the compile jail is the only writable one
		s.compiles++
		if s.failCompile {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "echo 'synthetic: syntax error' >&2; exit 1"), nil
		}
		_ = os.WriteFile(filepath.Join(spec.WorkDir, defaultArtifact), []byte("artifact"), 0o755)
		return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0"), nil
	}
	s.runs++
	return exec.CommandContext(ctx, "/bin/cat"), nil
}
func (s *countingSandbox) NetworkIsolated() bool { return false }
func (s *countingSandbox) Backend() string       { return "fake" }

func countingBatchService(sb *countingSandbox) *Service {
	return NewService(
		Limits{
			DefaultTimeout: 8000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512,
			DefaultCompileTimeout: 10000, MaxCompileTimeoutMs: 20000, MaxBatchTotalMs: 60000,
		},
		NewC(sb, CompiledConfig{OutputLimit: 64 * 1024, MaxProcesses: 256, MaxFileSizeMB: 64, CompileMemoryMB: 512}),
	)
}

// TestCompiledBatch_CompilesOnce is the compiled batch acceptance: exactly ONE
// compile jail for N inputs, one run jail per input, results index-aligned, and
// the compile telemetry on the envelope, never on the per-input results.
func TestCompiledBatch_CompilesOnce(t *testing.T) {
	sb := &countingSandbox{}
	res, err := countingBatchService(sb).RunBatch(context.Background(), runnerapi.RunRequest{
		Language:   "c",
		SourceCode: "int main(void){return 0;}",
		Stdins:     []string{"x", "y", "z"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if res.Status != runnerapi.BatchStatusOK || res.Aborted {
		t.Fatalf("batch envelope wrong: %+v", res)
	}
	if sb.compiles != 1 {
		t.Fatalf("batch must compile exactly once, compiled %d times", sb.compiles)
	}
	if sb.runs != 3 || len(res.Results) != 3 {
		t.Fatalf("expected 3 run jails / results, got %d / %d", sb.runs, len(res.Results))
	}
	for i, want := range []string{"x", "y", "z"} {
		r := res.Results[i]
		if r.Status != runnerapi.StatusSuccess || r.Stdout != want {
			t.Fatalf("results[%d]: status=%q stdout=%q want %q", i, r.Status, r.Stdout, want)
		}
		if r.CompileMs != 0 {
			t.Fatalf("results[%d] carries compile telemetry; it belongs on the envelope only", i)
		}
	}
}

// TestCompiledBatch_CompileErrorFailsWhole pins that a compile failure ends the
// batch once: compile_error on the envelope, diagnostics in CompileOutput, no
// run jail launched, and no per-input results.
func TestCompiledBatch_CompileErrorFailsWhole(t *testing.T) {
	sb := &countingSandbox{failCompile: true}
	res, err := countingBatchService(sb).RunBatch(context.Background(), runnerapi.RunRequest{
		Language:   "c",
		SourceCode: "int main(void){ this does not compile",
		Stdins:     []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if res.Status != runnerapi.StatusCompileError {
		t.Fatalf("expected compile_error envelope, got %+v", res)
	}
	if sb.runs != 0 || len(res.Results) != 0 {
		t.Fatalf("compile_error batch must execute nothing: runs=%d results=%d", sb.runs, len(res.Results))
	}
	if !strings.Contains(res.CompileOutput, "syntax error") {
		t.Fatalf("compiler diagnostics missing: %q", res.CompileOutput)
	}
}
