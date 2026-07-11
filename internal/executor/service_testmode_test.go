package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// wantValidation asserts err is a *ValidationError carrying the given stable code.
func wantValidation(t *testing.T, err error, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	if ve.Code != code {
		t.Fatalf("expected validation code %q, got %q (%s)", code, ve.Code, ve.Msg)
	}
}

func TestService_RejectsUnknownMode(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "python", SourceCode: "x", Mode: "grade"})
	wantValidation(t, err, "unsupported_mode")
}

// TestService_RejectsTestModeUnsupportedLanguage: a language with no test command
// registered (here a stub named "lua") cannot take mode=test — 400 before any run.
func TestService_RejectsTestModeUnsupportedLanguage(t *testing.T) {
	svc := newService(&stubRuntime{lang: "lua"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "lua", SourceCode: "x", Mode: "test"})
	wantValidation(t, err, "unsupported_test_mode")
}

// TestService_TestModeClampsToTestCeiling: a mode=test request clamps to the
// separate (larger) test-timeout envelope, not the run-mode ceiling.
func TestService_TestModeClampsToTestCeiling(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := NewService(Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000,
		DefaultMemory: 128, MaxMemoryMB: 512,
		DefaultTestTimeout: 15000, MaxTestTimeoutMs: 30000,
	}, stub)
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Mode: "test", TimeoutMs: 999999,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 30000 {
		t.Fatalf("test-mode timeout must clamp to the test ceiling 30000, got %d", stub.got.TimeoutMs)
	}
}

// TestService_TestModeDefaultTimeout: a mode=test request that omits a timeout
// gets the test default, not the run default.
func TestService_TestModeDefaultTimeout(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := NewService(Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000,
		DefaultMemory: 128, MaxMemoryMB: 512,
		DefaultTestTimeout: 15000, MaxTestTimeoutMs: 30000,
	}, stub)
	if _, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "python", SourceCode: "x", Mode: "test"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 15000 {
		t.Fatalf("test-mode omitted timeout must default to 15000, got %d", stub.got.TimeoutMs)
	}
}

// TestService_RejectsTestModeBatch: mode=test with a stdins[] batch is rejected —
// a test suite defines its own inputs and returns one report.
func TestService_RejectsTestModeBatch(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.RunBatch(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "x", Mode: "test", Stdins: []string{"a", "b"},
	})
	wantValidation(t, err, "test_mode_batch")
}

// TestService_RunModeUnchangedByDefault: a request that omits mode still clamps to
// the RUN-mode ceiling even when test ceilings are configured — run mode is
// untouched by G9.
func TestService_RunModeUnchangedByDefault(t *testing.T) {
	stub := &stubRuntime{lang: "python"}
	svc := NewService(Limits{
		DefaultTimeout: 3000, MaxTimeoutMs: 10000,
		DefaultMemory: 128, MaxMemoryMB: 512,
		DefaultTestTimeout: 15000, MaxTestTimeoutMs: 30000,
	}, stub)
	if _, err := svc.Run(context.Background(), runnerapi.RunRequest{Language: "python", SourceCode: "x", TimeoutMs: 999999}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.got.TimeoutMs != 10000 {
		t.Fatalf("run-mode timeout must clamp to the run ceiling 10000, got %d", stub.got.TimeoutMs)
	}
}

// conftestFiles is a files[] payload with pytest's conftest.py fixture — banned
// in run mode, admitted in test mode.
func conftestFiles() []runnerapi.RunFile {
	return []runnerapi.RunFile{
		{Path: "conftest.py", Content: "import pytest\n"},
		{Path: "test_x.py", Content: "def test_ok():\n    assert True\n"},
	}
}

// TestService_TestPolicyAdmitsConftest: the separate test policy admits conftest.py
// (which the run policy forbids) while the request validates cleanly.
func TestService_TestPolicyAdmitsConftest(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	if _, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", Mode: "test", Files: conftestFiles(),
	}); err != nil {
		t.Fatalf("test mode must admit conftest.py, got %v", err)
	}
}

// TestService_RunPolicyStillRejectsConftest: run mode is unchanged — conftest.py
// remains forbidden, so the test policy is a separate admission, never a relaxation.
func TestService_RunPolicyStillRejectsConftest(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", Files: conftestFiles(),
	})
	wantValidation(t, err, "forbidden_file")
}

// TestService_TestPolicyKeepsManifestBan: even in test mode a build/dependency
// manifest (setup.py) stays forbidden — the admission is scoped to test fixtures.
func TestService_TestPolicyKeepsManifestBan(t *testing.T) {
	svc := newService(&stubRuntime{lang: "python"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "python", Mode: "test", Files: []runnerapi.RunFile{
			{Path: "setup.py", Content: "from setuptools import setup\n"},
			{Path: "test_x.py", Content: "def test_ok():\n    assert True\n"},
		},
	})
	wantValidation(t, err, "forbidden_file")
}
