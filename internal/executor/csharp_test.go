package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestCSharpSpec pins the G15 C# runtime shape: a Shape-C compile (csc-direct to an
// IL Main.dll, OFFLINE — no NuGet restore) whose RUN posture is Java's — the CoreCLR
// via `dotnet exec` on the FULL rootfs, no RLIMIT_AS, denylist, a memory floor.
// Spec-level like the other compiled specs; the real jail compile+run is proven
// on-target in the CI Docker smoke and the image's build-time compile+run proof.
func TestCSharpSpec(t *testing.T) {
	joined := strings.Join(csharpSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{out}") || !strings.Contains(joined, "{dir}") {
		t.Fatalf("csharp compile must template {src}/{out}/{dir}: %v", csharpSpec.compile)
	}
	// csc-direct + the baked ref response file — the offline invariant (no MSBuild,
	// no restore). The prelude also copies the baked runtimeconfig beside Main.dll.
	if !strings.Contains(joined, "csc.dll") || !strings.Contains(joined, "@/opt/cs/refs.rsp") {
		t.Fatalf("csharp must csc-direct against the baked ref response file (offline): %v", csharpSpec.compile)
	}
	if !strings.Contains(joined, "Main.runtimeconfig.json") {
		t.Fatalf("csharp compile must place the runtimeconfig beside the assembly (dotnet exec needs it): %v", csharpSpec.compile)
	}
	// The compile prelude is /bin/sh (compileArgv0Absolute), so the && chain runs.
	if !csharpSpec.compileArgv0Absolute || csharpSpec.compile[0] != "/bin/sh" {
		t.Fatalf("csharp compile must run via an absolute /bin/sh prelude: %v", csharpSpec.compile)
	}
	if csharpSpec.artifact != "Main.dll" {
		t.Fatalf("csharp artifact must be Main.dll, got %q", csharpSpec.artifact)
	}
	// RUN = the CoreCLR: `dotnet exec Main.dll`, full rootfs, no RLIMIT_AS (like the JVM).
	if strings.Join(csharpSpec.run, " ") != "exec {out}" {
		t.Fatalf("csharp run must be `dotnet exec {out}`: %v", csharpSpec.run)
	}
	if len(csharpSpec.runBin) == 0 || csharpSpec.runBin[0] != "dotnet" {
		t.Fatalf("csharp runBin must resolve dotnet: %v", csharpSpec.runBin)
	}
	if !csharpSpec.runFullRootfs {
		t.Fatal("csharp must keep the full rootfs at run (the CoreCLR is dynamically linked)")
	}
	if csharpSpec.capAddressSpace {
		t.Fatal("csharp must NOT cap address space (the CLR reserves a virtual arena, like the JVM)")
	}
	if csharpSpec.staticAllowlistOK {
		t.Fatal("csharp must stay on the denylist (the CLR's JIT/clone syscall surface is wide)")
	}
	if csharpSpec.memErrSubstr == "" {
		t.Fatal("csharp needs a no-cgroup OOM marker (the CLR throws OutOfMemoryException)")
	}
	// The CLR's non-heap overhead means an undersized budget fails at startup — floor it.
	if csharpSpec.minMemoryMB == 0 {
		t.Fatal("csharp needs a memory floor for CLR non-heap overhead (like Java)")
	}
	// Offline hardening must be pinned in the run env (no telemetry / diagnostics IPC).
	runEnv := strings.Join(csharpSpec.runEnv, " ")
	if !strings.Contains(runEnv, "DOTNET_ROOT=/opt/dotnet") || !strings.Contains(runEnv, "DOTNET_EnableDiagnostics=0") {
		t.Fatalf("csharp run env must set DOTNET_ROOT and disable the diagnostic IPC: %v", csharpSpec.runEnv)
	}
	// W^X must be disabled on BOTH phases: .NET 8's W^X JIT ftruncates a 2 TB sparse
	// file that trips the jail's RLIMIT_FSIZE (SIGXFSZ), so csc AND the run JIT die
	// without this. A regression here reintroduces a bogus compile_error/runtime_error.
	compileEnv := strings.Join(csharpSpec.compileEnv, " ")
	if !strings.Contains(compileEnv, "DOTNET_EnableWriteXorExecute=0") {
		t.Fatalf("csharp compile env must disable W^X (2 TB ftruncate trips RLIMIT_FSIZE): %v", csharpSpec.compileEnv)
	}
	if !strings.Contains(runEnv, "DOTNET_EnableWriteXorExecute=0") {
		t.Fatalf("csharp run env must disable W^X (the run JIT trips RLIMIT_FSIZE too): %v", csharpSpec.runEnv)
	}
	// Invariant globalization on BOTH phases: the base image has no libicu, so the CLR
	// aborts on any globalization (even csc formatting an exception) without it. Also
	// the runner's culture-independence guarantee. A regression reintroduces the ICU crash.
	if !strings.Contains(compileEnv, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1") {
		t.Fatalf("csharp compile env must set invariant globalization (no libicu in base): %v", csharpSpec.compileEnv)
	}
	if !strings.Contains(runEnv, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1") {
		t.Fatalf("csharp run env must set invariant globalization (LC_ALL triggers the ICU load): %v", csharpSpec.runEnv)
	}
	// Workstation GC on BOTH phases: csc defaults to Server GC (a per-core heap), which
	// cannot initialize under the constrained, /proc-less compile jail — CoreCLR aborts
	// at "GC heap initialization failed with error 0x8007000E" before csc runs. A
	// regression here reintroduces that compile_error. The compile phase also pins an
	// explicit heap hard limit so sizing is /proc-independent (nsjail --disable_proc).
	if !strings.Contains(compileEnv, "DOTNET_gcServer=0") {
		t.Fatalf("csharp compile env must force Workstation GC (Server GC fails GC heap init in the jail): %v", csharpSpec.compileEnv)
	}
	if !strings.Contains(runEnv, "DOTNET_gcServer=0") {
		t.Fatalf("csharp run env must force Workstation GC (CLR startup under a /proc-less, cgroup-less jail): %v", csharpSpec.runEnv)
	}
	if !strings.Contains(compileEnv, "DOTNET_GCHeapHardLimit=") {
		t.Fatalf("csharp compile env must pin an explicit GC heap hard limit (/proc-independent sizing): %v", csharpSpec.compileEnv)
	}
	if csharpSpec.parseVersion == nil || csharpSpec.parseVersion("8.0.422\n") != "8.0.422" {
		t.Fatal("csharp version parse must extract the bare dotnet version")
	}
}

// TestCSharpJailConcessions pins the two limits the CoreCLR cannot start under,
// on BOTH phases (csc is itself a .NET program). capAddressSpace:false only
// declines to ADD a cap; nsjail's own 4 GB default still aborted the CLR at GC
// heap init, and its 32-descriptor default made the loader fail with a bogus
// "Could not load file or assembly 'System.Console'". Both were misdiagnosed as a
// missing assembly for exactly as long as the diagnostic below was on.
func TestCSharpJailConcessions(t *testing.T) {
	if !csharpSpec.unlimitedAddressSpace {
		t.Fatal("csharp must lift RLIMIT_AS outright: nsjail's 4 GB default aborts CoreCLR GC heap init (0x8007000E)")
	}
	if csharpSpec.maxOpenFiles < 1024 {
		t.Fatalf("csharp needs RLIMIT_NOFILE well above nsjail's 32 (one mmap per assembly), got %d", csharpSpec.maxOpenFiles)
	}
	// The COREHOST_TRACE diagnostic dumped ~64 KB of host resolution logging into
	// compile_output, overflowing its cap and SWALLOWING the real compiler error.
	// It must never ship again on either phase.
	for _, env := range [][]string{csharpSpec.compileEnv, csharpSpec.runEnv} {
		if strings.Contains(strings.Join(env, " "), "COREHOST_TRACE") {
			t.Fatalf("COREHOST_TRACE floods compile_output past its cap and hides the real error: %v", env)
		}
	}
	// Lifting RLIMIT_AS removed the only per-run memory bound in the no-cgroup mode,
	// so the ceiling must live in the runtime instead — C#'s -Xmx, templated per
	// request. A literal (untemplated) limit would silently ignore memory_mb.
	if !strings.Contains(strings.Join(csharpSpec.runEnv, " "), "DOTNET_GCHeapHardLimit={memhex}") {
		t.Fatalf("csharp run env must template the heap ceiling from the request budget: %v", csharpSpec.runEnv)
	}
	// ...and the fallback marker must match what breaching that ceiling actually
	// PRINTS. A GCHeapHardLimit breach fail-fasts with "Out of memory." on stderr; it
	// never throws OutOfMemoryException, so pinning the exception name would classify
	// every C# OOM as a plain runtime_error.
	if csharpSpec.memErrSubstr != "Out of memory" {
		t.Fatalf("csharp OOM marker must match the CLR's fail-fast text, got %q", csharpSpec.memErrSubstr)
	}
}

// TestMemHex pins the DOTNET_GCHeapHardLimit rendering: the CLR wants a hex BYTE
// count, so a megabyte budget must be shifted, not printed. A non-positive budget
// renders empty rather than a 0-byte heap that would fail every run.
func TestMemHex(t *testing.T) {
	for _, tc := range []struct {
		mb   int
		want string
	}{
		{128, "0x8000000"},
		{256, "0x10000000"},
		{512, "0x20000000"},
		{0, ""},
		{-1, ""},
	} {
		if got := memHex(tc.mb); got != tc.want {
			t.Fatalf("memHex(%d) = %q, want %q", tc.mb, got, tc.want)
		}
	}
}

// TestCSharp_MultiFileRejected pins the phase-1 scope: C# is source_code-only (a
// single Main.cs), so a files[] csharp request is a clean 400 (no csharp file policy
// — multi-class/namespace layouts across .cs files are a documented G3 follow-up),
// not a runtime failure.
func TestCSharp_MultiFileRejected(t *testing.T) {
	svc := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		&stubRuntime{lang: "csharp"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "csharp",
		Files:    []runnerapi.RunFile{{Path: "Main.cs", Content: "System.Console.WriteLine(1);"}},
	})
	wantValidation(t, err, "unsupported_language")
}
