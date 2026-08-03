package executor

import (
	"strings"
	"testing"
)

// TestOdinSpec pins the Odin runtime shape (B.1): a native single-file build via
// `odin build -file`, run as the artifact on the full-rootfs denylist jail. This
// is a SPEC-LEVEL test like the other compiled specs — the real jail compile+run
// is proven on-target by the CI Docker smoke, because the Odin toolchain is not
// present in the dev/unit-test image.
func TestOdinSpec(t *testing.T) {
	joined := strings.Join(odinSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{out}") {
		t.Fatalf("odin compile must template {src} and {out}: %v", odinSpec.compile)
	}
	// -file builds the single source rather than a package directory.
	if !strings.Contains(joined, "-file") {
		t.Fatalf("odin single-file build must pass -file: %v", odinSpec.compile)
	}
	if odinSpec.compile[0] != "odin" || odinSpec.compile[1] != "build" {
		t.Fatalf("odin compile must invoke `odin build`: %v", odinSpec.compile)
	}
	if strings.Join(odinSpec.run, " ") != "{out}" {
		t.Fatalf("odin run must be exactly the artifact: %v", odinSpec.run)
	}
	if odinSpec.sourceFile != "main.odin" {
		t.Fatalf("odin source file must be main.odin, got %q", odinSpec.sourceFile)
	}
	// A default odin build links libc dynamically → keep the full rootfs at run.
	if !odinSpec.runFullRootfs {
		t.Fatal("odin must keep the full rootfs (dynamic libc link) until a static build is verified")
	}
	// LLVM runtime syscall surface is wider than the C allowlist → denylist first.
	if odinSpec.staticAllowlistOK {
		t.Fatal("odin must stay on the denylist first (LLVM runtime syscalls)")
	}
	// Decided ON TARGET, and about the COMPILER rather than the program: the
	// artifact tolerates a hard RLIMIT_AS, but this flag gates both phases and
	// `odin build` embeds LLVM — under the compile jail's cap it panics with
	// "Out of Virtual Memory", which turned EVERY odin request into a
	// compile_error. Flipping this back re-breaks the language completely.
	if odinSpec.capAddressSpace {
		t.Fatal("odin must NOT cap address space: the LLVM-backed compiler panics under RLIMIT_AS (like `go build`)")
	}
	if len(odinSpec.binNames) == 0 || odinSpec.binNames[0] != "odin" {
		t.Fatalf("odin binNames must resolve `odin`: %v", odinSpec.binNames)
	}
	if odinSpec.parseVersion == nil {
		t.Fatal("odin must supply a version parser")
	}
}

func TestParseOdinVersion(t *testing.T) {
	cases := map[string]string{
		"odin version dev-2024-05:abc123": "dev-2024-05:abc123",
		"odin version 0.13.0":             "0.13.0",
		"dev-2024-05":                     "dev-2024-05", // no "version" word → last field
		"":                                "",
	}
	for in, want := range cases {
		if got := parseOdinVersion(in); got != want {
			t.Fatalf("parseOdinVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOdinTraceRecipe pins the B.2 step-through recipe. Everything here is a
// security or correctness property that is invisible in a unit test run (there
// is no gdb, no jail) but decides what happens on target, so it is pinned by
// shape.
func TestOdinTraceRecipe(t *testing.T) {
	tc, ok := traceCommands["odin"]
	if !ok {
		t.Fatal("odin must have a trace recipe (B.2)")
	}
	if !tc.isCompiledTrace() {
		t.Fatal("odin's trace is the COMPILED shape: a tracer over an artifact, not an interpreter over source")
	}
	if len(tc.tracerBin) == 0 || tc.tracerBin[0] != "gdb" {
		t.Fatalf("odin must trace under gdb, got %v", tc.tracerBin)
	}
	// -debug is the whole precondition: no DWARF, no line table, no locals — the
	// tutor would step a binary it cannot read.
	if strings.Join(tc.compileExtra, " ") != "-debug" {
		t.Fatalf("odin's trace build must force -debug, got %v", tc.compileExtra)
	}
	// -nx: a submission must never get to script the debugger through a .gdbinit
	// it dropped in the workdir.
	joined := strings.Join(tc.argvTail, " ")
	if !strings.Contains(joined, "-nx") {
		t.Fatalf("gdb must run with -nx so a workdir .gdbinit cannot script it: %v", tc.argvTail)
	}
	if !strings.Contains(joined, "--batch") {
		t.Fatalf("gdb must run in batch mode (never an interactive prompt): %v", tc.argvTail)
	}
	// The student breaks on THEIR main, not the Odin runtime's C entrypoint.
	if tc.entrySymbol != "main::main" {
		t.Fatalf("odin's entry symbol must be main::main, got %q", tc.entrySymbol)
	}
	// The driver filename must not be shadowable by a submission's own file.
	if !strings.HasPrefix(tc.harnessName, "_dalivim_") {
		t.Fatalf("the driver filename must be runner-fixed and unshadowable, got %q", tc.harnessName)
	}
	if tc.traceFormat != "dalivim-trace-json@2" {
		t.Fatalf("odin emits the v2 object-graph format, got %q", tc.traceFormat)
	}
	env := strings.Join(tc.env("main.odin", "", "trace.json", 100, 1000), " ")
	for _, want := range []string{"DALIVIM_TRACE_TARGET=main.odin", "DALIVIM_TRACE_ENTRY=main::main", "DALIVIM_TRACE_MAX_STEPS=100"} {
		if !strings.Contains(env, want) {
			t.Fatalf("trace env must carry %q: %s", want, env)
		}
	}
}

// TestOdinTraceDriver_HidesUndefinedVariables is the pedagogy guard. A native
// local exists (in DWARF) for the whole frame but holds stack GARBAGE until its
// initialising line runs — measured on target as n = 140737488344096 before
// `n := 3`. Showing that teaches the student something false, so the driver
// compares the current line against the declaring line. This test pins that the
// rule is STRICTLY greater and that arguments are gated on the function's line.
func TestOdinTraceDriver_HidesUndefinedVariables(t *testing.T) {
	if !strings.Contains(traceDriverGDB, "current_line <= decl") {
		t.Fatal("the driver must hide a local until execution has PASSED its declaring line")
	}
	if !strings.Contains(traceDriverGDB, "current_line <= func_line") {
		t.Fatal("the driver must hide arguments until the prologue is past the function's line")
	}
	// A pointer is never dereferenced: a dangling one is how a hostile program
	// would crash the tracer.
	if !strings.Contains(traceDriverGDB, "TYPE_CODE_PTR") || !strings.Contains(traceDriverGDB, "0x%x") {
		t.Fatal("the driver must report a pointer as an address, never dereference it")
	}
	if !strings.Contains(traceDriverGDB, "IMPLICIT_NAMES") {
		t.Fatal("the driver must drop compiler-injected names (Odin's `context`)")
	}
}
