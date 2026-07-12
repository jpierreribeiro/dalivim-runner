package executor

import (
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestRustSpec pins the G13 Rust runtime shape: a STATIC musl build (so the run
// jail keeps the minimal rootfs), WITH RLIMIT_AS (Rust tolerates it, unlike Go),
// on the denylist. Spec-level like the other compiled specs — the real jail
// compile+run is proven on-target in the CI Docker smoke (the {out}/{src} paths
// are in-jail /sandbox paths).
func TestRustSpec(t *testing.T) {
	joined := strings.Join(rustSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{out}") {
		t.Fatalf("rust compile must template {src} and {out}: %v", rustSpec.compile)
	}
	// musl target is the static invariant that keeps the run jail minimal-rootfs.
	if !strings.Contains(joined, "x86_64-unknown-linux-musl") {
		t.Fatalf("rust must target musl for a static binary: %v", rustSpec.compile)
	}
	if !strings.Contains(joined, "--edition 2021") {
		t.Fatalf("rust must pin --edition 2021 for determinism: %v", rustSpec.compile)
	}
	if strings.Join(rustSpec.run, " ") != "{out}" {
		t.Fatalf("rust run must be exactly the artifact: %v", rustSpec.run)
	}
	// Rust uses the system allocator, so a hard RLIMIT_AS bounds it (unlike Go).
	if !rustSpec.capAddressSpace {
		t.Fatal("rust must cap address space: it tolerates RLIMIT_AS (verified)")
	}
	// Rust std's syscall surface is wider than the C allowlist → denylist first.
	if rustSpec.staticAllowlistOK {
		t.Fatal("rust must stay on the denylist first (std uses futex/getrandom/…)")
	}
	if rustSpec.memErrSubstr == "" {
		t.Fatal("rust needs a no-cgroup OOM marker (Rust aborts with 'memory allocation of …')")
	}
	if len(rustSpec.binNames) == 0 || rustSpec.binNames[0] != "rustc" {
		t.Fatalf("rust binNames must resolve rustc: %v", rustSpec.binNames)
	}
	if rustSpec.parseVersion == nil || rustSpec.parseVersion("rustc 1.83.0 (abc)") != "1.83.0" {
		t.Fatal("rust version parse must extract the bare version (rustc X.Y.Z …)")
	}
	if rustSpec.compileTmpfsMB < 64 {
		t.Fatalf("rust compile /tmp must be roomy for the static link, got %dMB", rustSpec.compileTmpfsMB)
	}
}

// TestRustPolicy proves the rust file policy admits .rs and forbids the cargo/build
// surface (the runner compiles a single-file, std-only program with rustc, never
// cargo — a Cargo.toml or build.rs must be rejected).
func TestRustPolicy(t *testing.T) {
	p, ok := policyForMode("rust", modeRun, FileCaps{})
	if !ok {
		t.Fatal("rust must have a file policy")
	}
	if _, _, err := validateFiles(runnerapi.RunRequest{Language: "rust", Files: []runnerapi.RunFile{
		{Path: "main.rs", Content: "fn main(){ println!(\"4\"); }\n"},
	}}, p); err != nil {
		t.Fatalf("a plain main.rs must be accepted: %v", err)
	}
	for _, bad := range []string{"Cargo.toml", "build.rs"} {
		_, _, err := validateFiles(runnerapi.RunRequest{Language: "rust", Files: []runnerapi.RunFile{
			{Path: bad, Content: "x"},
			{Path: "main.rs", Content: "fn main(){}\n"},
		}}, p)
		if err == nil {
			t.Fatalf("%s must be forbidden in a rust submission", bad)
		}
	}
}
