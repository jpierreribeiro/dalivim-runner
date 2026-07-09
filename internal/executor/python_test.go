package executor

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// requirePython skips execution tests where python3 is unavailable, keeping
// `go test ./...` green on machines/CI without the interpreter.
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping runner execution test")
	}
}

// newPython builds a Python runtime with network isolation OFF so the test does
// not depend on the platform permitting unprivileged namespaces. The netns
// guarantees are exercised in netns_test.go.
func newPython(t *testing.T) *PythonRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewPython(sb, 64*1024)
}

func run(t *testing.T, rt *PythonRuntime, req runnerapi.RunRequest) runnerapi.RunResult {
	t.Helper()
	return rt.Run(context.Background(), req)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestPython_Success(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "print('hello')", TimeoutMs: 3000, MemoryMB: 128})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.Stdout != "hello\n" {
		t.Fatalf("expected stdout %q, got %q", "hello\n", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
}

func TestPython_Stdin(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{
		SourceCode: "import sys; sys.stdout.write(sys.stdin.read().upper())",
		Stdin:      "abc",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "ABC" {
		t.Fatalf("stdin echo failed: status=%q stdout=%q", res.Status, res.Stdout)
	}
}

func TestPython_RuntimeError(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "raise ValueError('boom')", TimeoutMs: 3000, MemoryMB: 128})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error, got %q", res.Status)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got 0")
	}
	if !strings.Contains(res.Stderr, "ValueError") {
		t.Fatalf("expected traceback with ValueError, got %q", res.Stderr)
	}
}

func TestPython_Timeout(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "while True:\n    pass", TimeoutMs: 500, MemoryMB: 128})
	if res.Status != runnerapi.StatusTimeout {
		t.Fatalf("expected timeout, got %q (stderr=%q)", res.Status, res.Stderr)
	}
}

func TestPython_OutputTruncation(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "print('a' * 200000)", TimeoutMs: 5000, MemoryMB: 128})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q", res.Status)
	}
	if !strings.HasSuffix(res.Stdout, "[output truncated]") {
		t.Fatalf("expected truncation marker, stdout tail=%q", tail(res.Stdout, 40))
	}
	if max := 64*1024 + len("\n[output truncated]"); len(res.Stdout) > max {
		t.Fatalf("stdout %d bytes exceeds cap %d", len(res.Stdout), max)
	}
}

// TestPython_MemoryExceededHeuristic pins the current, fragile classification:
// memory_exceeded is inferred purely from the substring "MemoryError" in stderr,
// not from a real memory signal. This characterizes today's behavior so its
// future replacement is a conscious change.
func TestPython_MemoryExceededHeuristic(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{
		SourceCode: "import sys; sys.stderr.write('MemoryError: fake'); sys.exit(1)",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusMemoryExceeded {
		t.Fatalf("expected memory_exceeded from stderr substring heuristic, got %q", res.Status)
	}
}

func TestPython_ReportsVersion(t *testing.T) {
	requirePython(t)
	rt := newPython(t)
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(rt.Version()) {
		t.Fatalf("expected a version like 3.x, got %q", rt.Version())
	}
}

func TestPython_AppliesCPULimit(t *testing.T) {
	requirePython(t)
	// The child should see RLIMIT_CPU = (timeout_ms/1000)+1 seconds (dash -t).
	src := "import resource\nprint(resource.getrlimit(resource.RLIMIT_CPU)[0])"
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: src, TimeoutMs: 3000, MemoryMB: 128})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "4" { // (3000+999)/1000 + 1 = 4
		t.Fatalf("CPU rlimit not applied, stdout=%q", res.Stdout)
	}
}

func TestPython_StderrCleanOfUlimitErrors(t *testing.T) {
	requirePython(t)
	// Regression: dash rejects `ulimit -u`; that must not leak into stderr.
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "import sys; sys.stderr.write('boom')", TimeoutMs: 3000, MemoryMB: 128})
	if strings.Contains(res.Stderr, "ulimit") || strings.Contains(res.Stderr, "Illegal option") {
		t.Fatalf("shell ulimit error leaked into stderr: %q", res.Stderr)
	}
	if res.Stderr != "boom" {
		t.Fatalf("stderr should be exactly the program's output, got %q", res.Stderr)
	}
}
