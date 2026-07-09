package executor

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// requireLua skips execution tests where no Lua interpreter is available, keeping
// `go test ./...` green on machines/CI without it.
func requireLua(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lua5.4"); err == nil {
		return
	}
	if _, err := exec.LookPath("lua"); err != nil {
		t.Skip("lua not available; skipping runner execution test")
	}
}

// newLua builds a Lua runtime with network isolation OFF so the test does not
// depend on the platform permitting unprivileged namespaces (mirrors newPython).
// The netns guarantees are exercised language-agnostically in netns_test.go.
func newLua(t *testing.T) *interpretedRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewLua(sb, 64*1024, 256, 64)
}

func TestLua_Success(t *testing.T) {
	requireLua(t)
	res := run(t, newLua(t), runnerapi.RunRequest{SourceCode: "print('hello')", TimeoutMs: 3000, MemoryMB: 128})
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

func TestLua_Stdin(t *testing.T) {
	requireLua(t)
	res := run(t, newLua(t), runnerapi.RunRequest{
		SourceCode: "io.write(io.read('a'):upper())",
		Stdin:      "abc",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "ABC" {
		t.Fatalf("stdin echo failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

func TestLua_RuntimeError(t *testing.T) {
	requireLua(t)
	res := run(t, newLua(t), runnerapi.RunRequest{SourceCode: "error('boom')", TimeoutMs: 3000, MemoryMB: 128})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got 0")
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("expected error text with boom, got %q", res.Stderr)
	}
}

func TestLua_Timeout(t *testing.T) {
	requireLua(t)
	res := run(t, newLua(t), runnerapi.RunRequest{SourceCode: "while true do end", TimeoutMs: 500, MemoryMB: 128})
	if res.Status != runnerapi.StatusTimeout {
		t.Fatalf("expected timeout, got %q (stderr=%q)", res.Status, res.Stderr)
	}
}

// TestLua_MemoryExceededHeuristic mirrors the Python characterization: with no
// delegated cgroup, memory_exceeded is inferred from Lua's "not enough memory" OOM
// marker in stderr (memErrSubstr). Authoritative real-bomb containment (RLIMIT_AS +
// cgroup) is proven on-target by the escape corpus, not here.
func TestLua_MemoryExceededHeuristic(t *testing.T) {
	requireLua(t)
	res := run(t, newLua(t), runnerapi.RunRequest{
		SourceCode: "io.stderr:write('not enough memory'); os.exit(1)",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusMemoryExceeded {
		t.Fatalf("expected memory_exceeded from stderr substring heuristic, got %q", res.Status)
	}
}

func TestLua_ReportsVersion(t *testing.T) {
	requireLua(t)
	rt := newLua(t)
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(rt.Version()) {
		t.Fatalf("expected a version like 5.x, got %q", rt.Version())
	}
}
