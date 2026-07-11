package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestGoTestCommand_Spec pins the Go test-mode recipe (G9). Like the other
// compiled specs it is a spec-level test — the real jail execution is proven
// on-target in the CI Docker smoke, since the Go toolchain and the pre-warmed
// /opt/gocache live only in the image.
func TestGoTestCommand_Spec(t *testing.T) {
	tc, ok := compiledTestCommands["go"]
	if !ok {
		t.Fatal("go must be registered in compiledTestCommands")
	}
	joined := strings.Join(tc.argv, " ")
	// Single jail: seed the pre-warmed cache (as the compile jail) and run go test.
	if !strings.Contains(joined, "/opt/gocache") {
		t.Fatalf("go test must seed GOCACHE from /opt/gocache: %v", tc.argv)
	}
	// -trimpath is load-bearing: it MUST match the image's -trimpath warm cache
	// (Dockerfile) or `go test` hits none of it and cold-rebuilds the stdlib past
	// the run wall. Pin it so a future edit can't silently drop it.
	for _, want := range []string{"go test", "-trimpath", "-json", "-p 1", "-count=1", "./..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("go test argv must contain %q: %v", want, tc.argv)
		}
	}
	// Offline + read-only module policy so a self-contained module never writes and
	// an external import fails closed.
	env := strings.Join(tc.env, " ")
	for _, want := range []string{"GOPROXY=off", "GOFLAGS=-mod=readonly", "GOCACHE=/tmp/gocache", "GOTOOLCHAIN=local"} {
		if !strings.Contains(env, want) {
			t.Fatalf("go test env must pin %q: %v", want, tc.env)
		}
	}
	if tc.reportFormat != "go-test-json" {
		t.Fatalf("go report_format must be go-test-json, got %q", tc.reportFormat)
	}
	if tc.testsFailedExit != 1 {
		t.Fatalf("go tests-failed exit must be 1, got %d", tc.testsFailedExit)
	}
	if tc.buildFailMarker != `"Action":"build-fail"` {
		t.Fatalf("go build-fail marker must be the go test -json build-fail action, got %q", tc.buildFailMarker)
	}
	if tc.tmpfsMB < 128 {
		t.Fatalf("go test /tmp must be roomy for GOCACHE, got %dMB", tc.tmpfsMB)
	}
	if !strings.HasSuffix(tc.sourceFile, "_test.go") {
		t.Fatalf("go source_code test file must end in _test.go (discovery), got %q", tc.sourceFile)
	}
}

// TestGoTestCommand_ModPrep proves the module file is synthesized at the source
// root (go test needs a module) and is the local, network-free module.
func TestGoTestCommand_ModPrep(t *testing.T) {
	tc := compiledTestCommands["go"]
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, srcRootName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tc.prep(workDir); err != nil {
		t.Fatalf("prep: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(workDir, srcRootName, "go.mod"))
	if err != nil {
		t.Fatalf("go.mod not synthesized: %v", err)
	}
	if !strings.Contains(string(b), "module sandbox.local/") {
		t.Fatalf("synthesized go.mod must be the local module, got: %s", b)
	}
}

// TestClassifyTestExit pins the shared exit→status mapping used by every test-mode
// language: 0=success, the framework's tests-failed code (with a report)=
// tests_failed, everything else defers to the caller (runtime_error / a language
// override like go's build-fail → compile_error).
func TestClassifyTestExit(t *testing.T) {
	cases := []struct {
		exit, failExit int
		produced       bool
		want           string
	}{
		{0, 1, true, runnerapi.StatusSuccess},
		{0, 1, false, runnerapi.StatusSuccess}, // exit 0 is success even without a report
		{1, 1, true, runnerapi.StatusTestsFailed},
		{1, 1, false, ""}, // exit 1 but no report → not a clean test-fail
		{2, 1, true, ""},  // pytest collection error / other → caller decides
		{5, 1, true, ""},  // pytest no-tests
	}
	for _, c := range cases {
		if got := classifyTestExit(c.exit, c.failExit, c.produced); got != c.want {
			t.Fatalf("classifyTestExit(%d,%d,%v)=%q want %q", c.exit, c.failExit, c.produced, got, c.want)
		}
	}
}

// TestGoTestPolicy_AdmitsTestFiles proves the go test file policy admits .go /
// *_test.go while forbidding a caller-supplied go.mod (the runner synthesizes it).
func TestGoTestPolicy_AdmitsTestFiles(t *testing.T) {
	p, ok := policyForMode("go", modeTest, FileCaps{})
	if !ok {
		t.Fatal("go must have a test file policy")
	}
	ok1 := []runnerapi.RunFile{
		{Path: "solution.go", Content: "package solution\nfunc Add(a,b int) int { return a+b }\n"},
		{Path: "solution_test.go", Content: "package solution\nimport \"testing\"\nfunc TestA(t *testing.T){ if Add(1,1)!=2 {t.Fatal(\"x\")} }\n"},
	}
	if _, err := validateTestFiles(runnerapi.RunRequest{Language: "go", Mode: "test", Files: ok1}, p); err != nil {
		t.Fatalf("valid go test tree rejected: %v", err)
	}
	bad := []runnerapi.RunFile{
		{Path: "go.mod", Content: "module x\n"},
		{Path: "solution_test.go", Content: "package solution\n"},
	}
	if _, err := validateTestFiles(runnerapi.RunRequest{Language: "go", Mode: "test", Files: bad}, p); err == nil {
		t.Fatal("a caller-supplied go.mod must be forbidden in go test mode")
	}
}
