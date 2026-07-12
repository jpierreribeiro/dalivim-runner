package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestTypeScriptSpec pins the G14 TypeScript runtime shape: a Shape-C compile step
// (tsc type-checks + emits main.js) whose RUN posture is JavaScript's, verbatim —
// node on the FULL rootfs, V8 heap bounded (no RLIMIT_AS), denylist. Spec-level
// like the other compiled specs; the real jail compile+run is proven on-target in
// the CI Docker smoke (the {out}/{dir}/{src} paths are in-jail /sandbox paths).
func TestTypeScriptSpec(t *testing.T) {
	joined := strings.Join(typescriptSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{dir}") {
		t.Fatalf("typescript compile must template {src} and {dir} (outDir): %v", typescriptSpec.compile)
	}
	// --noEmitOnError is the invariant that makes a type error a compile_error:
	// nothing is emitted, so no artifact reaches the run jail.
	if !strings.Contains(joined, "--noEmitOnError") {
		t.Fatalf("typescript must pin --noEmitOnError (type error -> compile_error): %v", typescriptSpec.compile)
	}
	// --strict is the point of TypeScript — the runner guarantees full type-checking.
	if !strings.Contains(joined, "--strict") {
		t.Fatalf("typescript must pin --strict: %v", typescriptSpec.compile)
	}
	// @types/node must be wired in, or Node globals (console/process/…) don't type-check.
	if !strings.Contains(joined, "--types node") || !strings.Contains(joined, "/opt/ts-types") {
		t.Fatalf("typescript must resolve bundled @types/node via --types node --typeRoots: %v", typescriptSpec.compile)
	}
	// The emitted artifact is main.js (tsc emits it from main.ts), run by node.
	if typescriptSpec.artifact != "main.js" {
		t.Fatalf("typescript artifact must be main.js, got %q", typescriptSpec.artifact)
	}
	// RUN reuses the JavaScript posture: full rootfs (node is dynamically linked) and
	// NO RLIMIT_AS (V8 reserves a virtual cage), bounded by the heap flag + cgroup.
	if !typescriptSpec.runFullRootfs {
		t.Fatal("typescript must keep the full rootfs at run (node is dynamically linked)")
	}
	if typescriptSpec.capAddressSpace {
		t.Fatal("typescript must NOT cap address space (V8's virtual cage refuses RLIMIT_AS)")
	}
	if typescriptSpec.staticAllowlistOK {
		t.Fatal("typescript must stay on the denylist (node's syscall surface is wide)")
	}
	// The run argv is node's: the __proto__ hardening flag, the heap bound, the artifact.
	runJoined := strings.Join(typescriptSpec.run, " ")
	if !strings.Contains(runJoined, "--max-old-space-size={mem}") || !strings.Contains(runJoined, "{out}") {
		t.Fatalf("typescript run must bound the V8 heap and run the artifact: %v", typescriptSpec.run)
	}
	if len(typescriptSpec.runBin) == 0 || typescriptSpec.runBin[0] != "node" {
		t.Fatalf("typescript runBin must resolve node: %v", typescriptSpec.runBin)
	}
	if typescriptSpec.parseVersion == nil || typescriptSpec.parseVersion("Version 5.9.3") != "5.9.3" {
		t.Fatal("typescript version parse must extract the bare version (Version X.Y.Z)")
	}
}

// TestTypeScript_MultiFileRejected pins the phase-1 scope: TypeScript is
// source_code-only (a single main.ts), so a files[] typescript request is a clean
// 400 (no typescript file policy — sibling .ts imports need a fixed multi-file
// emit/run layout, a documented follow-up), not a runtime failure.
func TestTypeScript_MultiFileRejected(t *testing.T) {
	svc := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		&stubRuntime{lang: "typescript"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "typescript",
		Files:    []runnerapi.RunFile{{Path: "main.ts", Content: "console.log(1);"}},
	})
	wantValidation(t, err, "unsupported_language")
}
