package executor

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

func testCompileLimits() CompileLimits {
	return CompileLimits{MemoryMB: 512, DefaultTimeoutMs: 15000, MaxTimeoutMs: 30000, MaxArtifactBytes: 50_000_000}
}

// requireCC / requireCpp skip when the compiler is unavailable, keeping
// `go test ./...` green without a toolchain.
func requireCC(t *testing.T) {
	t.Helper()
	if resolveBin(cSpec.compilerNames) == "" {
		t.Skip("no C compiler; skipping C runtime test")
	}
}

func requireCpp(t *testing.T) {
	t.Helper()
	if resolveBin(cppSpec.compilerNames) == "" {
		t.Skip("no C++ compiler; skipping C++ runtime test")
	}
}

func newC(t *testing.T) *compiledRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewC(sb, 64*1024, 256, 64, testCompileLimits())
}

func newCpp(t *testing.T) *compiledRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewCpp(sb, 64*1024, 256, 64, testCompileLimits())
}

func runC(t *testing.T, rt *compiledRuntime, req runnerapi.RunRequest) runnerapi.RunResult {
	t.Helper()
	return rt.Run(context.Background(), req)
}

func TestC_Success(t *testing.T) {
	requireCC(t)
	res := runC(t, newC(t), runnerapi.RunRequest{
		SourceCode: "#include <stdio.h>\nint main(void){ puts(\"42\"); return 0; }",
		TimeoutMs:  5000, MemoryMB: 256, CompileTimeoutMs: 10000,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (compile=%q stderr=%q)", res.Status, res.CompileOutput, res.Stderr)
	}
	if res.Stdout != "42\n" {
		t.Fatalf("expected stdout %q, got %q", "42\n", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

func TestC_Stdin(t *testing.T) {
	requireCC(t)
	src := "#include <stdio.h>\nint main(void){ int a,b; if(scanf(\"%d %d\",&a,&b)==2) printf(\"%d\\n\", a+b); return 0; }"
	res := runC(t, newC(t), runnerapi.RunRequest{SourceCode: src, Stdin: "20 22", TimeoutMs: 5000, MemoryMB: 256})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "42\n" {
		t.Fatalf("stdin add failed: status=%q stdout=%q compile=%q", res.Status, res.Stdout, res.CompileOutput)
	}
}

func TestC_CompileError(t *testing.T) {
	requireCC(t)
	// Undeclared identifier => compile fails; nothing must run.
	res := runC(t, newC(t), runnerapi.RunRequest{
		SourceCode: "int main(void){ return nope; }",
		TimeoutMs:  5000, MemoryMB: 256,
	})
	if res.Status != runnerapi.StatusCompileError {
		t.Fatalf("expected compile_error, got %q", res.Status)
	}
	if !strings.Contains(res.CompileOutput, "nope") && !strings.Contains(strings.ToLower(res.CompileOutput), "error") {
		t.Fatalf("expected compiler diagnostic in compile_output, got %q", res.CompileOutput)
	}
	if res.Stdout != "" {
		t.Fatalf("a compile_error must not produce program stdout, got %q", res.Stdout)
	}
}

func TestC_RuntimeError(t *testing.T) {
	requireCC(t)
	res := runC(t, newC(t), runnerapi.RunRequest{
		SourceCode: "int main(void){ return 3; }",
		TimeoutMs:  5000, MemoryMB: 256,
	})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error, got %q (compile=%q)", res.Status, res.CompileOutput)
	}
	if res.ExitCode != 3 {
		t.Fatalf("expected exit code 3, got %d", res.ExitCode)
	}
}

func TestC_Timeout(t *testing.T) {
	requireCC(t)
	// `while(1){}` has a constant controlling expression, so C forbids the compiler
	// from optimising the loop away — it really spins.
	res := runC(t, newC(t), runnerapi.RunRequest{
		SourceCode: "int main(void){ while(1){} return 0; }",
		TimeoutMs:  500, MemoryMB: 256,
	})
	if res.Status != runnerapi.StatusTimeout {
		t.Fatalf("expected timeout, got %q (compile=%q)", res.Status, res.CompileOutput)
	}
}

func TestC_ReportsVersion(t *testing.T) {
	requireCC(t)
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(newC(t).Version()) {
		t.Fatalf("expected a compiler version like 12.x, got %q", newC(t).Version())
	}
}

func TestCpp_Success(t *testing.T) {
	requireCpp(t)
	src := "#include <iostream>\nint main(){ std::cout << 6*7 << std::endl; return 0; }"
	res := runC(t, newCpp(t), runnerapi.RunRequest{SourceCode: src, TimeoutMs: 5000, MemoryMB: 256, CompileTimeoutMs: 20000})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "42\n" {
		t.Fatalf("expected success/42, got status=%q stdout=%q compile=%q", res.Status, res.Stdout, res.CompileOutput)
	}
}

// TestC_CompilerBombContained proves the compile phase is itself jailed: a
// preprocessor token-multiplication bomb (10^6 array elements — not memoised the
// way templates are) blows the compile timeout and is reported as compile_error,
// never a hung or OOM'd runner. This is the §4.4 "compiler bomb" corpus row.
func TestC_CompilerBombContained(t *testing.T) {
	requireCC(t)
	const bomb = "#define C0 1,1,1,1,1,1,1,1,1,1\n" +
		"#define C1 C0,C0,C0,C0,C0,C0,C0,C0,C0,C0\n" +
		"#define C2 C1,C1,C1,C1,C1,C1,C1,C1,C1,C1\n" +
		"#define C3 C2,C2,C2,C2,C2,C2,C2,C2,C2,C2\n" +
		"#define C4 C3,C3,C3,C3,C3,C3,C3,C3,C3,C3\n" +
		"#define C5 C4,C4,C4,C4,C4,C4,C4,C4,C4,C4\n" +
		"#define C6 C5,C5,C5,C5,C5,C5,C5,C5,C5,C5\n" +
		"int a[] = { C6 };\nint main(void){ return 0; }"
	res := runC(t, newC(t), runnerapi.RunRequest{SourceCode: bomb, TimeoutMs: 3000, MemoryMB: 256, CompileTimeoutMs: 2000})
	if res.Status != runnerapi.StatusCompileError {
		t.Fatalf("expected the compiler bomb to be contained as compile_error, got %q", res.Status)
	}
	if res.Stdout != "" {
		t.Fatalf("compiler bomb must not run anything, got stdout=%q", res.Stdout)
	}
}

// TestCompilerVersion exercises the pure version parser without a compiler.
func TestCompilerVersion(t *testing.T) {
	if got := compilerVersion("gcc (Debian 12.2.0-14) 12.2.0\nCopyright ...\n"); got != "12.2.0" {
		t.Fatalf("compilerVersion = %q", got)
	}
	if got := compilerVersion(""); got != "" {
		t.Fatalf("compilerVersion on empty = %q", got)
	}
}

func TestAppendNote(t *testing.T) {
	if got := appendNote("", "timed out"); got != "timed out" {
		t.Fatalf("appendNote empty = %q", got)
	}
	if got := appendNote("boom\n", "timed out"); got != "boom\ntimed out" {
		t.Fatalf("appendNote = %q", got)
	}
}
