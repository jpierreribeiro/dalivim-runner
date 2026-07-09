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

// requireNode skips execution tests where a Node interpreter is unavailable,
// keeping `go test ./...` green on machines/CI without it.
func requireNode(t *testing.T) {
	t.Helper()
	if resolveBin(javascriptSpec.binNames) == "" || detectVersion(resolveBin(javascriptSpec.binNames), javascriptSpec) == "" {
		t.Skip("node not available; skipping JavaScript execution test")
	}
}

// newNode builds a Node runtime with network isolation OFF so the test does not
// depend on the platform permitting unprivileged namespaces (the netns egress
// guarantee is language-agnostic and already covered in netns_test.go).
func newNode(t *testing.T) *interpretedRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewNode(sb, 64*1024, 256, 64)
}

func TestNode_Success(t *testing.T) {
	requireNode(t)
	// 8s (not 3s): a Node/V8 cold start under the race detector on a contended CI
	// runner can exceed a tight 3s budget and spuriously time out. The assertion is
	// on success+output, not latency, so a generous wall budget removes the flake.
	res := run(t, newNode(t), runnerapi.RunRequest{SourceCode: "console.log(2+2)", TimeoutMs: 8000, MemoryMB: 128})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.Stdout != "4\n" {
		t.Fatalf("expected stdout %q, got %q", "4\n", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
}

func TestNode_Stdin(t *testing.T) {
	requireNode(t)
	// Read all of stdin and echo it uppercased, with no trailing newline.
	src := "const s = require('fs').readFileSync(0, 'utf8'); process.stdout.write(s.toUpperCase());"
	res := run(t, newNode(t), runnerapi.RunRequest{SourceCode: src, Stdin: "abc", TimeoutMs: 8000, MemoryMB: 128})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "ABC" {
		t.Fatalf("stdin echo failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

func TestNode_RuntimeError(t *testing.T) {
	requireNode(t)
	res := run(t, newNode(t), runnerapi.RunRequest{SourceCode: "throw new Error('boom')", TimeoutMs: 8000, MemoryMB: 128})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got 0")
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("expected the thrown error in stderr, got %q", res.Stderr)
	}
}

func TestNode_Timeout(t *testing.T) {
	requireNode(t)
	res := run(t, newNode(t), runnerapi.RunRequest{SourceCode: "while (true) {}", TimeoutMs: 500, MemoryMB: 128})
	if res.Status != runnerapi.StatusTimeout {
		t.Fatalf("expected timeout, got %q (stderr=%q)", res.Status, res.Stderr)
	}
}

func TestNode_DisableProto(t *testing.T) {
	requireNode(t)
	// --disable-proto=throw makes any __proto__ access throw, so this run must
	// FAIL — proving the hardening flag reaches the interpreter.
	res := run(t, newNode(t), runnerapi.RunRequest{SourceCode: "console.log(({}).__proto__)", TimeoutMs: 8000, MemoryMB: 128})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected __proto__ access to throw under --disable-proto=throw, got status=%q stdout=%q", res.Status, res.Stdout)
	}
}

func TestNode_ReportsVersion(t *testing.T) {
	requireNode(t)
	rt := newNode(t)
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(rt.Version()) {
		t.Fatalf("expected a version like 20.x (leading 'v' stripped), got %q", rt.Version())
	}
}

// TestSpecRegistry_ArgvIsFileReferenced pins the security-critical invariant that
// no spec inlines source: the argv is fixed flags plus the source FILENAME, and
// the code is written to a file. Runs without any interpreter.
func TestSpecRegistry_ArgvIsFileReferenced(t *testing.T) {
	for _, spec := range []languageSpec{pythonSpec, javascriptSpec, luaSpec} {
		if spec.name == "" || spec.sourceFile == "" || len(spec.binNames) == 0 || len(spec.runArgs) == 0 {
			t.Fatalf("spec %+v is missing required fields", spec)
		}
		last := spec.runArgs[len(spec.runArgs)-1]
		if last != spec.sourceFile {
			t.Fatalf("%s: runArgs must end in the source filename %q, got %q", spec.name, spec.sourceFile, last)
		}
	}
}

// TestVersionParsers exercises the pure version parsing without an interpreter.
func TestVersionParsers(t *testing.T) {
	if got := secondField("Python 3.12.3\n"); got != "3.12.3" {
		t.Fatalf("secondField: got %q", got)
	}
	if got := secondField("weird"); got != "" {
		t.Fatalf("secondField on malformed output: got %q", got)
	}
	if got := trimLeadingV("v20.11.0\n"); got != "20.11.0" {
		t.Fatalf("trimLeadingV: got %q", got)
	}
	if got := secondField("Lua 5.4.6  Copyright (C) 1994-2023 Lua.org, PUC-Rio\n"); got != "5.4.6" {
		t.Fatalf("secondField on lua banner: got %q", got)
	}
}

// TestNode_InheritsNetnsJail proves JavaScript inherits the SAME containment as
// Python by flowing through the identical sandbox seam: an egress attempt from
// Node is refused by the empty network namespace, exactly as it is for Python
// (netns_test.go). Skips where unprivileged namespaces are unavailable.
func TestNode_InheritsNetnsJail(t *testing.T) {
	requireNode(t)
	sb, err := sandbox.Configure("off", "auto", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	if !sb.NetworkIsolated() {
		t.Skip("unprivileged network namespaces unavailable on this platform")
	}
	rt := NewNode(sb, 64*1024, 256, 64)
	src := "const net = require('net');\n" +
		"const s = net.connect({host: '1.1.1.1', port: 80});\n" +
		"s.on('connect', () => { console.log('OPEN'); process.exit(0); });\n" +
		"s.on('error', (e) => { console.log('blocked:' + e.code); process.exit(0); });\n"
	res := rt.Run(context.Background(), runnerapi.RunRequest{SourceCode: src, TimeoutMs: 5000, MemoryMB: 128})
	if strings.Contains(res.Stdout, "OPEN") {
		t.Fatalf("network isolation failed: JS reached the network (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "blocked") {
		t.Fatalf("expected a blocked-connection result, got status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestMemoryModel_PerLanguage pins the two enforcement paths: CPython takes a
// hard RLIMIT_AS and no heap flag; Node takes NO address-space cap and bounds its
// heap via --max-old-space-size derived from the budget. Runs without an
// interpreter.
func TestMemoryModel_PerLanguage(t *testing.T) {
	if !pythonSpec.capAddressSpace || pythonSpec.memoryArgs != nil {
		t.Fatalf("python must cap address space and pass no heap flag: cap=%v memoryArgs=%v",
			pythonSpec.capAddressSpace, pythonSpec.memoryArgs != nil)
	}
	if javascriptSpec.capAddressSpace {
		t.Fatal("javascript must NOT cap address space (V8 reserves a multi-GB virtual cage)")
	}
	if javascriptSpec.memoryArgs == nil {
		t.Fatal("javascript must bound its heap via memoryArgs")
	}
	if got := javascriptSpec.memoryArgs(128); len(got) != 1 || got[0] != "--max-old-space-size=128" {
		t.Fatalf("node heap flag = %v, want [--max-old-space-size=128]", got)
	}
}

// TestResolveBin_FallsBackToFirstCandidate proves boot never blocks on a missing
// interpreter: an all-unknown candidate list yields the first name, not "".
func TestResolveBin_FallsBackToFirstCandidate(t *testing.T) {
	if got := resolveBin([]string{"definitely-not-a-real-binary-xyz"}); got != "definitely-not-a-real-binary-xyz" {
		t.Fatalf("expected fallback to the first candidate name, got %q", got)
	}
	// Sanity: a real binary resolves to an absolute path.
	if _, err := exec.LookPath("sh"); err == nil {
		if got := resolveBin([]string{"sh"}); !strings.HasPrefix(got, "/") {
			t.Fatalf("expected an absolute path for sh, got %q", got)
		}
	}
}
