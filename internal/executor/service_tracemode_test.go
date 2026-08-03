package executor

import (
	"context"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestService_RejectsTraceModeUnsupportedLanguage: a language with no trace
// harness registered (a stub named "lua") cannot take mode=trace — 400 before any
// run, exactly like the test-mode gate.
func TestService_RejectsTraceModeUnsupportedLanguage(t *testing.T) {
	svc := newService(&stubRuntime{lang: "lua"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "lua", SourceCode: "x", Mode: "trace"})
	wantValidation(t, err, "unsupported_trace_mode")
}

// TestService_TraceModeClampsToTestCeiling: mode=trace reuses the larger
// test-timeout envelope (tracing is slower than a bare run), not the run ceiling.
func TestService_TraceModeClampsToTestCeiling(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := NewService(Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000,
		DefaultMemory: 128, MaxMemoryMB: 512,
		DefaultTestTimeout: 15000, MaxTestTimeoutMs: 30000,
	}, stub)
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Mode: "trace", TimeoutMs: 999999,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 30000 {
		t.Fatalf("trace-mode timeout must clamp to the test ceiling 30000, got %d", stub.got.TimeoutMs)
	}
}

// TestService_RejectsTraceModeBatch: mode=trace with a stdins[] batch is rejected
// — a trace run returns one step-through document, not a per-input matrix.
func TestService_RejectsTraceModeBatch(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Mode: "trace", Stdins: []string{"a", "b"},
	})
	wantValidation(t, err, "trace_mode_batch")
}

// TestService_CatalogAdvertisesTrace: the discovery catalog reports trace support
// for a trace-capable language (python) and not for one without a harness.
func TestService_CatalogAdvertisesTrace(t *testing.T) {
	svc := NewService(Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000,
		DefaultMemory: 128, MaxMemoryMB: 512,
	}, &stubRuntime{lang: "python"}, &stubRuntime{lang: "lua"})
	for _, info := range svc.Catalog() {
		switch info.ID {
		case "python":
			if !info.Trace {
				t.Fatalf("python must advertise trace support")
			}
		case "lua":
			if info.Trace {
				t.Fatalf("lua must not advertise trace support")
			}
		}
	}
}
