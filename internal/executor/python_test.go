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
func newPython(t *testing.T) *interpretedRuntime {
	t.Helper()
	// netns backend (RUNNER_SANDBOX=off) with network isolation off, so the test
	// depends on neither nsjail nor the platform permitting namespaces.
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewPython(sb, 64*1024, 256, 64, 4_000_000)
}

func run(t *testing.T, rt *interpretedRuntime, req runnerapi.RunRequest) runnerapi.RunResult {
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

// TestPython_OutputFloodKilled pins the locked G1.4 behavior: an unbounded print
// loop is KILLED once it crosses the output cap and classified
// output_limit_exceeded, rather than truncated-and-left-running to burn the whole
// timeout. The captured prefix (first cap bytes + marker) is still returned, and
// the run ends well before the deadline.
func TestPython_OutputFloodKilled(t *testing.T) {
	requirePython(t)
	res := run(t, newPython(t), runnerapi.RunRequest{SourceCode: "while True:\n    print('x' * 1024)", TimeoutMs: 5000, MemoryMB: 128})
	if res.Status != runnerapi.StatusOutputLimitExceeded {
		t.Fatalf("expected output_limit_exceeded, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if !strings.HasSuffix(res.Stdout, "[output truncated]") {
		t.Fatalf("expected truncation marker, stdout tail=%q", tail(res.Stdout, 40))
	}
	// The authoritative signal: the serialized bool, not the text marker. A caller
	// (the grader) reads this to decide output_limit without scanning the stream.
	if !res.StdoutTruncated {
		t.Fatal("StdoutTruncated must be true on a truncated stream")
	}
	if max := 64*1024 + len("\n[output truncated]"); len(res.Stdout) > max {
		t.Fatalf("stdout %d bytes exceeds cap %d", len(res.Stdout), max)
	}
	// Fail-fast: the kill lands far short of the 5s deadline.
	if res.DurationMs >= 4000 {
		t.Fatalf("expected fast kill well under timeout, got %dms", res.DurationMs)
	}
}

// TestPython_RequestOutputLimitHonored proves a per-request budget reaches the
// actual stream buffer when a Runtime is invoked directly.
func TestPython_RequestOutputLimitHonored(t *testing.T) {
	requirePython(t)
	const requested = 1024
	res := run(t, newPython(t), runnerapi.RunRequest{
		SourceCode: "print('x' * 8192)", TimeoutMs: 5000, MemoryMB: 128,
		OutputLimitBytes: requested,
	})
	if res.Status != runnerapi.StatusOutputLimitExceeded || !res.StdoutTruncated {
		t.Fatalf("requested output cap must kill and truncate, got status=%q truncated=%v", res.Status, res.StdoutTruncated)
	}
	if max := requested + len("\n[output truncated]"); len(res.Stdout) > max {
		t.Fatalf("stdout %d bytes exceeds requested cap %d", len(res.Stdout), max)
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

// TestPythonSpec_PinsHashSeed pins G11: PYTHONHASHSEED=0 must be in the Python run
// env so set/dict iteration order is stable run-to-run. A spec-level check (no
// interpreter needed) so the pin can't be dropped silently.
func TestPythonSpec_PinsHashSeed(t *testing.T) {
	found := false
	for _, e := range pythonSpec.env {
		if e == "PYTHONHASHSEED=0" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pythonSpec.env must pin PYTHONHASHSEED=0 (G11), got %v", pythonSpec.env)
	}
}

// TestPython_HashOrderDeterministic pins G11 behaviorally: the SAME program that
// prints a set of strings — whose iteration order depends on the hash seed —
// produces BYTE-IDENTICAL output across two independent runs (each a fresh
// interpreter process). Without PYTHONHASHSEED=0 the two processes would draw
// different random seeds and the order could differ; with it pinned they match.
func TestPython_HashOrderDeterministic(t *testing.T) {
	requirePython(t)
	rt := newPython(t)
	// A 50-element string set: with random seeding the printed order almost always
	// differs between two processes; pinned, it is identical.
	src := "print(list({'k' + str(i) for i in range(50)}))"
	req := runnerapi.RunRequest{SourceCode: src, TimeoutMs: 3000, MemoryMB: 128}
	a := run(t, rt, req)
	b := run(t, rt, req)
	if a.Status != runnerapi.StatusSuccess || b.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q / %q", a.Status, b.Status)
	}
	if a.Stdout != b.Stdout {
		t.Fatalf("set iteration order not deterministic across runs:\n  run1=%s  run2=%s", a.Stdout, b.Stdout)
	}
}
