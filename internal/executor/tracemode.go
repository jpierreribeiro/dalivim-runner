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
}

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
}

// supportsTraceMode reports whether a language has a trace-mode command
// registered (G16). The service gates a mode=trace request on this and the
// catalog advertises it via LanguageInfo.Trace.
func supportsTraceMode(lang string) bool {
	_, ok := traceCommands[lang]
	return ok
}
