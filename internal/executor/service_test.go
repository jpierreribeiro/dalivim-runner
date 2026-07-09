package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// stubRuntime records the request it received so limit-clamping can be asserted
// without executing anything.
type stubRuntime struct {
	lang string
	got  runnerapi.RunRequest
}

func (s *stubRuntime) Language() string { return s.lang }
func (s *stubRuntime) Version() string  { return "9.9.9" }
func (s *stubRuntime) Run(_ context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	s.got = req
	return runnerapi.RunResult{Status: runnerapi.StatusSuccess}
}

func newService(rt Runtime) *Service {
	return NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512}, rt)
}

func TestService_UnsupportedLanguage(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "ruby", SourceCode: "puts 1"})
	if !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("expected ErrUnsupportedLanguage, got %v", err)
	}
}

func TestService_ClampsLimitsToCeiling(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := newService(stub)
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", TimeoutMs: 999999, MemoryMB: 999999,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 10000 {
		t.Fatalf("timeout not clamped to max, got %d", stub.got.TimeoutMs)
	}
	if stub.got.MemoryMB != 512 {
		t.Fatalf("memory not clamped to max, got %d", stub.got.MemoryMB)
	}
}

// TestService_ClampsCompileTimeout pins G1.2: compile_timeout_ms is now honoured
// but clamped in the service layer — a request may lower the compile bound but
// never raise it past the ceiling, and zero falls back to the default.
func TestService_ClampsCompileTimeout(t *testing.T) {
	stub := &stubRuntime{lang: "c"}
	limits := Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512,
		DefaultCompileTimeout: 10000, MaxCompileTimeoutMs: 20000,
	}
	svc := NewService(limits, stub)

	cases := []struct{ in, want int }{
		{0, 10000},     // omitted → default
		{5000, 5000},   // below ceiling → honoured (lowers the bound)
		{99999, 20000}, // above ceiling → clamped down (never raises it)
	}
	for _, c := range cases {
		if _, err := svc.Run(context.Background(), runnerapi.RunRequest{
			Language: "c", SourceCode: "int main(){}", CompileTimeoutMs: c.in,
		}); err != nil {
			t.Fatalf("run(compile_timeout_ms=%d): %v", c.in, err)
		}
		if stub.got.CompileTimeoutMs != c.want {
			t.Fatalf("compile_timeout_ms %d clamped to %d, want %d", c.in, stub.got.CompileTimeoutMs, c.want)
		}
	}
}

func TestService_AppliesDefaultsAndProvenance(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := newService(stub)
	res, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "python", SourceCode: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 3000 || stub.got.MemoryMB != 128 {
		t.Fatalf("defaults not applied: %+v", stub.got)
	}
	if res.RuntimeName != "python" || res.RuntimeVersion != "9.9.9" {
		t.Fatalf("provenance not stamped: %+v", res)
	}
	if res.PythonVersion != "9.9.9" {
		t.Fatalf("deprecated python_version alias not set, got %q", res.PythonVersion)
	}
}
