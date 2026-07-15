package executor

import (
	"io"
	"os"
	"path/filepath"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

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

	// sourceFile, when set, overrides the run-mode spec.sourceFile as the filename a
	// single source_code test submission is written to, so the framework's discovery
	// matches it. node --test only runs files matching its test-name pattern, so a JS
	// source_code test is written to main.test.js; pytest collects from an
	// explicitly-named file, so python leaves this empty and reuses main.py. A
	// files[] submission ignores it (the backend names its own files).
	sourceFile string
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
	"javascript": {
		// node --disable-proto=throw --test --test-reporter=tap, with NO positional
		// path: node's built-in runner DISCOVERS test files recursively from the jail
		// cwd (/sandbox) — it finds src/*.test.js for a files[] tree and main.test.js
		// at the root for a source_code submission. A positional DIRECTORY is not
		// recursed by node 18's runner (it is treated as a single test file and
		// errors), so discovery — not a positional arg — is the portable form; the
		// argvTail therefore ignores target. --disable-proto=throw mirrors run mode's
		// prototype-pollution hardening. Built-in runner => no node_modules, so the
		// offline posture needs no extra policy beyond the empty netns.
		//
		// The TAP report is on STDOUT (reportFile ""), like go test -json. Node exits
		// 1 both when a test fails AND when a test file throws while loading (e.g. a
		// student module with a syntax error): unlike pytest's distinct exit-2
		// collection error, node folds a load failure into its report as a failing
		// test, so such a run classifies tests_failed with the error visible in the
		// raw TAP — the honest, dumb-runner relay of node's own verdict. A crash before
		// any TAP is produced still falls through to runtime_error (no report).
		argvTail: func(_, _ string) []string {
			return []string{"--disable-proto=throw", "--test", "--test-reporter=tap"}
		},
		env: determinismEnv(
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=/nonexistent", // no user config (.node_repl_history, etc.)
		),
		reportFormat:    "tap13",
		reportFile:      "", // TAP on stdout
		testsFailedExit: 1,
		sourceFile:      "main.test.js", // so node's discovery matches a source_code test
	},
}

// compiledTestCommand is the CLOSED test-mode recipe for a COMPILED language
// (G9). It runs the whole compile+test in ONE jail via a /bin/sh prelude, so it
// avoids run mode's two-jail split: a toolchain's own `test` subcommand does both
// (`go test`), and a `javac && java-junit` prelude does both for Java. It is
// edited deliberately here, never request-driven.
type compiledTestCommand struct {
	// argv is the full jail argv — a /bin/sh -c prelude (argv[0] absolute, execve'd
	// directly) that compiles then runs the tests. No student token appears in it. It
	// may carry a {mem} placeholder that executeTest substitutes with the run's
	// memory budget (Java's -Xmx heap flag; go has none).
	argv []string

	// env is the toolchain environment: the determinism pin plus the offline module
	// policy and the seeded GOCACHE/GOPATH (as the compile jail uses).
	env []string

	// reportFormat labels the report (ReportFormat on the result); the report is on
	// stdout for `go test -json`.
	reportFormat string

	// testsFailedExit is the exit code meaning "ran to completion, >=1 test failed"
	// (`go test` = 1) — the gate for tests_failed.
	testsFailedExit int

	// tmpfsMB sizes the writable /tmp the toolchain needs for GOCACHE + build output
	// (as the compile jail: a cold stdlib build writes tens of MB).
	tmpfsMB int

	// buildFailMarker, when non-empty and present in the report, means the toolchain
	// failed to COMPILE the submission. `go test -json` emits an
	// `"Action":"build-fail"` event and exits 1 — the SAME exit code as a real test
	// failure — so exit code alone cannot tell a build failure from a test failure.
	// This structured, documented marker (not a stderr guess) is the authoritative
	// signal that routes a build failure to compile_error instead of tests_failed,
	// matching go's run-mode semantics.
	buildFailMarker string

	// prep runs on the host after materialization and before the jail — synthesize
	// the module file (go.mod) at the source root, mirroring the multi-file build.
	prep func(workDir string) error

	// sourceFile is the filename a single source_code test submission is written to
	// under the source root. For go it MUST end in _test.go (main_test.go) so
	// `go test` discovers it; for java the public test class must match it
	// (MainTest.java); a files[] submission ignores this.
	sourceFile string

	// reportFile is the workdir-relative path the framework writes its machine-
	// readable report to, read back from the writable /sandbox bind. "" means the
	// report is on the process STDOUT (go test -json) and is read from the captured
	// stream instead. Java writes JUnit XML to a file, so it sets this; go leaves it "".
	reportFile string

	// writable binds /sandbox READ-WRITE so the framework can hand back files the
	// host must read (Java: the compiled classes and the JUnit XML report). go leaves
	// it false — `go test`'s report is on stdout and its build output goes to the
	// private /tmp tmpfs, so /sandbox stays read-only.
	writable bool

	// compileFailExit, when non-zero, is the exit code the prelude uses to signal a
	// COMPILE failure (Java's `javac … || exit 42`) — a code the test launcher never
	// returns — so the runner routes it to compile_error, distinct from a genuine
	// test failure. go leaves it 0 and uses buildFailMarker (its build failure shares
	// exit 1 with a test failure, so it needs the structured report marker instead).
	compileFailExit int
}

// compiledTestCommands is the closed compiled-language test registry (G9), keyed
// by language and populated in init below. A language here supports mode=test via
// the single-toolchain-jail path; a language in testCommands supports it via the
// interpreted path. supportsTestMode unions the two.
var compiledTestCommands = map[string]*compiledTestCommand{}

func init() {
	compiledTestCommands["go"] = goTestCommand()
	compiledTestCommands["java"] = javaTestCommand()
}

// junitConsoleJar is the in-image path of the bundled JUnit Platform Console
// Standalone jar (launcher + Jupiter/Vintage engines) — pinned in the Dockerfile.
const junitConsoleJar = "/opt/junit/junit-console.jar"

// javaTestCommand builds Java's test recipe (G9). Java is VM-compiled, so a single
// /bin/sh prelude does BOTH phases in one jail: javac compiles {student + hidden
// test} .java (find handles nested packages) against the JUnit console jar into
// /sandbox/classes, then the JVM runs the console launcher, which DISCOVERS @Test
// methods on the classpath and writes JUnit XML into /sandbox/reports. /sandbox is
// WRITABLE so the classes and the report are host-visible for the hand-back (the
// jail /tmp is a private tmpfs); the host rootfs stays read-only.
//
// `|| exit 42` is the compile-failure SENTINEL: a javac failure short-circuits the
// chain with a code the JVM launcher never returns, so the runner routes it to
// compile_error (java's run-mode status), kept distinct from a genuine test failure
// (exit 1 + a produced report). The JVM opts out of RLIMIT_AS (executeTest sets it
// 0) and is bounded by -Xmx{mem} + the cgroup, exactly like the Java run jail; the
// determinism flags mirror run mode (SerialGC, no perf-data, one processor).
func javaTestCommand() *compiledTestCommand {
	sh := "mkdir -p /sandbox/classes /sandbox/reports && " +
		"javac -encoding UTF-8 -cp " + junitConsoleJar + " -d /sandbox/classes $(find /sandbox/src -name '*.java') || exit 42; " +
		"exec java -Xmx{mem}m -XX:+UseSerialGC -XX:-UsePerfData -XX:ActiveProcessorCount=1 " +
		"-jar " + junitConsoleJar + " execute --class-path /sandbox/classes --scan-class-path " +
		"--reports-dir=/sandbox/reports --disable-banner --details=none"
	return &compiledTestCommand{
		argv:            []string{"/bin/sh", "-c", sh},
		env:             determinismEnv("PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent"),
		reportFormat:    "junit-xml",
		reportFile:      "reports/TEST-junit-jupiter.xml", // the JUnit 5 engine's legacy XML
		testsFailedExit: 1,
		compileFailExit: 42,
		writable:        true,            // classes + report hand-back on /sandbox
		sourceFile:      "MainTest.java", // source_code: public test class must be MainTest
	}
}

// goTestCommand builds Go's test recipe. `go test` compiles AND runs in one jail,
// so — unlike run mode's split compile/run jails — this is a single full-rootfs,
// toolchain-present, writable-/tmp jail (the compile-jail posture, kept alive to
// run). It seeds the pre-warmed /opt/gocache exactly as the compile prelude does,
// pins single-package parallelism + a fresh (uncached) run for determinism, and
// stays offline. The report is `go test -json` on stdout.
//
// -trimpath is REQUIRED, not cosmetic: the image warms /opt/gocache with -trimpath
// (Dockerfile) and it is part of Go's build-cache key, so a `go test` WITHOUT it
// hits zero of the warm cache and cold-rebuilds the whole stdlib single-threaded —
// which blows the run wall (SIGKILL/timeout). It also matches run mode's
// `go build -trimpath`, keeping the toolchain determinism (G7) consistent.
func goTestCommand() *compiledTestCommand {
	src := srcJailDir() // /sandbox/src, the synthesized module root
	sh := "cp -r /opt/gocache /tmp/gocache && cd " + shellQuote(src) +
		" && exec go test -trimpath -json -p 1 -count=1 ./..."
	return &compiledTestCommand{
		argv: []string{"/bin/sh", "-c", sh},
		// Determinism pin (G7) + the toolchain env: seeded GOCACHE/GOPATH on the
		// size-capped /tmp, local toolchain (no network fetch), and the offline
		// module policy (-mod=readonly so a self-contained stdlib module never tries
		// to write go.mod/go.sum into the read-only /sandbox; an external import fails
		// closed as a build error). GOMAXPROCS=1 keeps scheduler churn low under the
		// denylist + pids cap.
		env: determinismEnv(
			"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp",
			"CGO_ENABLED=0", "GOCACHE=/tmp/gocache", "GOPATH=/tmp/gopath",
			"GOTOOLCHAIN=local", "GOENV=off",
			"GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=-mod=readonly",
			"GOMAXPROCS=1",
		),
		reportFormat:    "go-test-json",
		testsFailedExit: 1,
		tmpfsMB:         256,
		buildFailMarker: `"Action":"build-fail"`,
		prep: func(workDir string) error {
			return os.WriteFile(filepath.Join(workDir, srcRootName, "go.mod"), []byte(goModContent), 0o600)
		},
		sourceFile: "main_test.go",
	}
}

// supportsTestMode reports whether a language has a test-mode command registered
// (G9), in either the interpreted or the compiled registry. The service gates a
// mode=test request on this, and the catalog advertises it via LanguageInfo.Test.
func supportsTestMode(lang string) bool {
	if _, ok := testCommands[lang]; ok {
		return true
	}
	_, ok := compiledTestCommands[lang]
	return ok
}

// classifyTestExit maps a completed test run to its terminal status, given the
// framework's exit code, the exit code that means "ran, some failed", and whether
// a report was actually produced. It is the ONE addition to the run-mode
// classification switch, evaluated after timeout/memory/output but before the
// generic runtime_error default:
//
//   - exit == 0 && report               → success (all tests passed)
//   - exit == testsFailedExit && report  → tests_failed (ran, some failed)
//   - anything else                      → "" (caller falls through to runtime_error)
//
// The report-produced guard applies to BOTH pass and fail: untrusted student code
// runs inside the framework process and can terminate it with exit(0) before any
// test executes. An exit code without a completed report is a harness crash, never
// proof that tests passed. A collection/import error (pytest exit 2) never matches
// testsFailedExit, so it stays runtime_error even though pytest also emits a report
// for it. (A go BUILD failure also exits 1, so the compiled path checks its
// buildFailMarker BEFORE calling this — see the compiled executeTest.)
func classifyTestExit(exitCode, testsFailedExit int, reportProduced bool) string {
	switch {
	case exitCode == 0 && reportProduced:
		return runnerapi.StatusSuccess
	case exitCode == testsFailedExit && reportProduced:
		return runnerapi.StatusTestsFailed
	default:
		return "" // not a test-mode terminal state; caller decides (runtime_error)
	}
}

// readTestReport returns the framework's report (G9): the contents of its report
// file under workDir (read back from the writable /sandbox bind), or the given
// stdout for a framework that reports there (reportFile == ""). It reports whether
// a non-empty report was actually produced — the authoritative "the suite ran"
// signal — and caps the report at maxBytes independently of the stdout limit,
// flagging a cut. Shared by the interpreted and compiled test paths.
//
// File-backed reports are attacker-writable: student code and the framework run
// under the same jail uid with /sandbox writable. Never use os.ReadFile on a path
// from that tree. A malicious test could replace report.xml with a symlink to
// /proc/self/environ; once the jail exits, a host-side path lookup would follow it
// in the RUNNER process namespace and disclose RUNNER_SERVICE_TOKEN. os.Root keeps
// every lookup beneath workDir, Lstat rejects a final symlink/special file, and the
// SameFile check closes the lookup/open replacement race. The bounded reader also
// prevents a max-file-size report from being allocated before truncation.
func readTestReport(workDir, reportFile, stdout string, maxBytes int) (report string, produced, truncated bool) {
	if reportFile == "" {
		if maxBytes > 0 && len(stdout) > maxBytes {
			return stdout[:maxBytes], true, true
		}
		return stdout, stdout != "", false
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return "", false, false
	}
	defer root.Close()

	before, err := root.Lstat(reportFile)
	if err != nil || !before.Mode().IsRegular() {
		return "", false, false
	}
	f, err := root.Open(reportFile)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return "", false, false
	}

	var reader io.Reader = f
	if maxBytes > 0 {
		reader = io.LimitReader(f, int64(maxBytes)+1)
	}
	b, err := io.ReadAll(reader)
	if err != nil || len(b) == 0 {
		return "", false, false
	}
	if maxBytes > 0 && len(b) > maxBytes {
		return string(b[:maxBytes]), true, true
	}
	return string(b), true, false
}
