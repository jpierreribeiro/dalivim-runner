package executor

import "github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"

// modeRun and modeTest are the two execution shapes (G9). The empty string is an
// alias for modeRun so a request that omits mode takes the unchanged run path.
const (
	modeRun  = "run"
	modeTest = "test"
)

// testCommand is the CLOSED per-language test-mode recipe (G9): the fixed
// framework command and how its machine-readable report is produced. Like the
// language and file-policy registries it is edited DELIBERATELY, never driven by
// request input — a free-form command would be arbitrary execution and break the
// "dumb + safe" contract. The framework judges its own assertions; the runner
// only transcribes the report it wrote.
type testCommand struct {
	// argvTail returns the interpreter/tool argv tail (everything after the
	// resolved binary) for a test run over target — the in-workdir path the
	// framework should discover tests under ("src" for a files[] tree, the single
	// source filename for source_code). It contains only fixed tokens and target,
	// never any student-supplied string, so there is no argv injection.
	argvTail func(target, reportFile string) []string

	// env is the child environment: the shared determinism pin (G7) plus the
	// framework's offline/determinism hardening. Never inherits the host env.
	env []string

	// reportFormat labels the report the backend must parse (ReportFormat on the
	// result). Opaque to the runner.
	reportFormat string

	// reportFile is the workdir-relative path the framework writes its report to.
	// It is read back from the writable /sandbox bind (the same host-visible
	// hand-back the compile phase uses for its artifact — the jail's /tmp is a
	// private tmpfs the host cannot read). Empty means the report is the process
	// stdout (TAP / go test -json), read from the captured stream instead.
	reportFile string

	// testsFailedExit is the framework exit code meaning "ran to completion, >=1
	// test failed" (pytest 1, go test 1, node --test 1, JUnit 1) — the gate for
	// StatusTestsFailed, distinct from a crash exit. Any other non-zero code
	// (collection/usage error, no tests, signal) falls through to runtime_error.
	testsFailedExit int
}

// testCommands is the closed test-mode registry keyed by language (G9). A
// language absent here does not support mode=test; the service rejects such a
// request with 400 before anything runs. Populated one language at a time behind
// the same mode=test flag — python (pytest) is the reference; go/js/java follow.
var testCommands = map[string]testCommand{
	"python": {
		// python3 -s -P -m pytest <flags> <target>. -s -P (NOT -I) mirrors run mode:
		// it keeps the user-site + cwd/script-dir hardening while still honouring the
		// PYTHONHASHSEED pin (-I's implied -E would ignore it — the G11 lesson). pytest
		// itself lives in system site-packages, unaffected by -s. -P keeps /sandbox
		// off sys.path so a student cannot plant a module at the workdir root to
		// hijack an import; pytest inserts the test file's own directory (src/) so a
		// hidden test's `import solution` resolves to the sibling student file.
		//
		//   -p no:cacheprovider  no .pytest_cache write (a built-in plugin, so it
		//                        loads even with autoload disabled below)
		//   --tb=short -q        compact, deterministic human summary on stdout
		//   --junit-xml=<file>   the machine-readable report, written into /sandbox
		argvTail: func(target, reportFile string) []string {
			return []string{
				"-s", "-P", "-m", "pytest",
				"-p", "no:cacheprovider",
				"--tb=short", "-q",
				"--junit-xml=" + reportFile,
				target,
			}
		},
		// Determinism pin + PYTHONHASHSEED=0 (as run mode), plus:
		//   PYTHONDONTWRITEBYTECODE=1  no __pycache__ writes into the tree (pytest's
		//                              assertion rewrite still works in-memory)
		//   PYTEST_DISABLE_PLUGIN_AUTOLOAD=1  no setuptools-entrypoint plugin loads
		//                              off the tree/site — closes the network/plugin
		//                              autoload surface. Built-in plugins (junitxml,
		//                              cacheprovider) are unaffected, so the report
		//                              is still produced.
		env: determinismEnv(
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=/nonexistent",
			"PYTHONUNBUFFERED=1",
			"PYTHONHASHSEED=0",
			"PYTHONDONTWRITEBYTECODE=1",
			"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1",
		),
		reportFormat:    "junit-xml",
		reportFile:      "report.xml",
		testsFailedExit: 1,
	},
}

// supportsTestMode reports whether a language has a test-mode command registered
// (G9). The service gates a mode=test request on this, and the catalog advertises
// it via LanguageInfo.Test.
func supportsTestMode(lang string) bool {
	_, ok := testCommands[lang]
	return ok
}

// classifyTestExit maps a completed test run to its terminal status, given the
// framework's exit code and whether a report was actually produced. It is the ONE
// addition to the run-mode classification switch, evaluated after timeout/memory/
// output but before the generic runtime_error default:
//
//   - exit == 0                          → success (all tests passed)
//   - exit == testsFailedExit && report  → tests_failed (ran, some failed)
//   - anything else                      → "" (caller falls through to runtime_error)
//
// The report-produced guard is the authoritative signal — the runner tells "tests
// failed" from "harness crashed" by the presence of a completed report, not by
// guessing from stderr. A collection/import error (pytest exit 2) never matches
// testsFailedExit, so it stays runtime_error even though pytest also emits a
// report for it.
func classifyTestExit(tc testCommand, exitCode int, reportProduced bool) string {
	switch {
	case exitCode == 0:
		return runnerapi.StatusSuccess
	case exitCode == tc.testsFailedExit && reportProduced:
		return runnerapi.StatusTestsFailed
	default:
		return "" // not a test-mode terminal state; caller decides (runtime_error)
	}
}
