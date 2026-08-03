package executor

import (
	_ "embed"
	"strconv"
)

// modeTrace is the third execution shape (G16), a sibling of modeRun/modeTest.
// It runs the student program under a language tracer and returns a BOUNDED,
// structured, per-line execution trace (current line, call stack, locals/globals
// per frame, cumulative stdout length, and where a crash happened) for a
// Python-Tutor-style step-through visualizer. Like modeTest it is a CLOSED,
// in-code, per-language capability — never driven by request input — and reuses
// the identical jail as run mode. The empty string / modeRun remain unchanged.
const modeTrace = "trace"

// traceHarnessPython is the fixed CPython tracer written into the jail alongside
// the student source. It runs the entrypoint under sys.settrace and transcribes
// a bounded JSON trace into the report file. It is embedded (not generated) so it
// is auditable in one place and covered by the harness security tests. The
// student code and its runtime values are treated as HOSTILE — see the file's
// header for the full bounds/serialization discipline.
//
//go:embed trace_harness.py
var traceHarnessPython string

// traceDriverGDB is the COMPILED-language driver (B.2). Same contract as the
// Python harness — bounded, hostile-input-safe, emits dalivim-trace-json@2 — but
// it drives gdb over a `-debug` artifact instead of hooking an interpreter.
//
//go:embed trace_driver_gdb.py
var traceDriverGDB string

// traceCommand is the CLOSED per-language trace recipe (G16), the trace-mode
// analogue of testCommand. It describes the fixed harness that produces the
// machine-readable trace and how it is invoked. It is edited DELIBERATELY here,
// one language at a time, never assembled from request input — a trace harness is
// arbitrary code running over untrusted values, so it must be a vetted constant.
type traceCommand struct {
	// harnessName is the filename the harness source is written to in the per-run
	// workdir (jail cwd). It is fixed and distinct from any student filename so a
	// submission cannot shadow it.
	harnessName string

	// harness is the harness program source (embedded above). The runner writes it
	// into the workdir before launching the interpreter over it.
	harness string

	// argvTail returns the interpreter argv tail (everything after the resolved
	// binary): the fixed interpreter flags plus the harness filename. It contains
	// only fixed tokens — the student entrypoint and the trace root are passed to
	// the harness via the controlled env below, never via argv, so there is no argv
	// injection.
	argvTail []string

	// env builds the child environment: the same determinism pin as run mode plus
	// the harness's bounds (target entrypoint, trace root, report path, step and
	// byte caps). target is the entrypoint relative to the jail cwd, root is the
	// directory whose frames are traced (both runner-derived, never student text),
	// and reportFile/maxSteps/maxBytes are runner policy.
	env func(target, root, reportFile string, maxSteps, maxBytes int) []string

	// traceFormat labels the trace report the backend/frontend must parse
	// (TraceFormat on the result). Opaque to the runner.
	traceFormat string

	// reportFile is the workdir-relative path the harness writes its JSON trace to,
	// read back from the writable /sandbox bind (the jail's /tmp is private). It is
	// read with the SAME traversal-resistant reader as the test report.
	reportFile string

	// ── COMPILED-language fields (B.2). Empty for an interpreted language. ──

	// tracerBin names the tracer binary to resolve on PATH (e.g. gdb). Non-empty
	// marks this entry as the compiled shape: the runner compiles first and then
	// runs the TRACER over the artifact, instead of running an interpreter over
	// the student's source.
	tracerBin []string

	// compileExtra is appended to the language's normal compile argv for a trace
	// build. For Odin this is `-debug`: without DWARF there are no line tables and
	// no locals, so the tutor has nothing to read. The runner controls the compile
	// line, so a debug build is guaranteed rather than requested.
	compileExtra []string

	// entrySymbol is the symbol the driver breaks on to start stepping — the
	// student's main, not the runtime's. Odin mangles it as `main::main` (the C
	// `main` is the runtime's entry_unix.odin bootstrap, which is not the
	// program the student wrote).
	entrySymbol string
}

// isCompiledTrace reports whether this recipe drives a tracer over a compiled
// artifact (B.2) rather than an interpreter over source.
func (t traceCommand) isCompiledTrace() bool { return len(t.tracerBin) > 0 }

// traceCommands is the closed trace-mode registry keyed by language (G16). A
// language absent here does not support mode=trace; the service rejects such a
// request with 400 before anything runs. Python (CPython sys.settrace) is the
// reference and, for this slice, the only entry — JS/others follow the same seam
// later.
var traceCommands = map[string]traceCommand{
	"python": {
		harnessName: "_dalivim_trace.py",
		harness:     traceHarnessPython,
		// python3 -s -P _dalivim_trace.py. Same -s -P hardening as run mode (drop
		// user-site, keep cwd/script-dir off sys.path, honour PYTHONHASHSEED — NOT -I,
		// whose implied -E would ignore the determinism pin). The harness reads the
		// student entrypoint from the env, so no student string appears in argv.
		argvTail: []string{"-s", "-P", "_dalivim_trace.py"},
		env: func(target, root, reportFile string, maxSteps, maxBytes int) []string {
			return determinismEnv(
				"PATH=/usr/local/bin:/usr/bin:/bin",
				"HOME=/nonexistent",
				"PYTHONUNBUFFERED=1",
				"PYTHONHASHSEED=0",
				"PYTHONDONTWRITEBYTECODE=1",
				"DALIVIM_TRACE_TARGET="+target,
				"DALIVIM_TRACE_ROOT="+root,
				"DALIVIM_TRACE_REPORT="+reportFile,
				"DALIVIM_TRACE_MAX_STEPS="+strconv.Itoa(maxSteps),
				"DALIVIM_TRACE_MAX_REPORT_BYTES="+strconv.Itoa(maxBytes),
			)
		},
		traceFormat: "dalivim-trace-json@1",
		reportFile:  "trace.json",
	},
	// Odin (B.2): the first COMPILED language with a step-through. There is no
	// sys.settrace for a native binary, so the recipe is compile-with-DWARF then
	// step under gdb. The jail it runs in is deliberately NOT the run jail — see
	// executeTrace: it is the only place the runner permits ptrace, and it pairs
	// that with a procfs of the jail's own PID namespace, because a debugger
	// cannot resolve a PIE load base without one. Everything else in the denylist
	// stays; the security review behind this is docs/future/B2-TRACER-SPIKE.md.
	"odin": {
		harnessName: "_dalivim_trace_gdb.py",
		harness:     traceDriverGDB,
		tracerBin:   []string{"gdb"},
		// --batch: run the script and exit, never an interactive prompt.
		// -nx: ignore any .gdbinit — the student's workdir must not script gdb.
		// --quiet: no banner on the student's stderr.
		argvTail:     []string{"--batch", "-nx", "--quiet", "-x", "_dalivim_trace_gdb.py", "./bin"},
		compileExtra: []string{"-debug"},
		entrySymbol:  "main::main",
		env: func(target, root, reportFile string, maxSteps, maxBytes int) []string {
			_ = root // the compiled driver filters by the student FILE, not a root dir
			return determinismEnv(
				"PATH=/usr/local/bin:/usr/bin:/bin",
				"HOME=/nonexistent",
				"TMPDIR=/tmp",
				"PYTHONDONTWRITEBYTECODE=1",
				"DALIVIM_TRACE_LANGUAGE=odin",
				"DALIVIM_TRACE_TARGET="+target,
				"DALIVIM_TRACE_ENTRY=main::main",
				"DALIVIM_TRACE_REPORT="+reportFile,
				"DALIVIM_TRACE_STDOUT="+traceStdoutFile,
				"DALIVIM_TRACE_STDERR="+traceStderrFile,
				"DALIVIM_TRACE_MAX_STEPS="+strconv.Itoa(maxSteps),
				"DALIVIM_TRACE_MAX_REPORT_BYTES="+strconv.Itoa(maxBytes),
			)
		},
		traceFormat: "dalivim-trace-json@2",
		reportFile:  "trace.json",
	},
}

// The traced program's own streams, in the compiled shape. They exist because
// the inferior shares the DEBUGGER's stdout: without redirecting it, gdb's stop
// announcements and stepped source lines land in the student's output. The
// driver redirects the inferior into these files and the runner reads them back
// as Stdout/Stderr, dropping gdb's stream. Runner-fixed names, prefixed like the
// driver so no submission can shadow them.
const (
	traceStdoutFile = "_dalivim_stdout.txt"
	traceStderrFile = "_dalivim_stderr.txt"
)

// supportsTraceMode reports whether a language has a trace-mode command
// registered (G16). The service gates a mode=trace request on this and the
// catalog advertises it via LanguageInfo.Trace.
func supportsTraceMode(lang string) bool {
	_, ok := traceCommands[lang]
	return ok
}
