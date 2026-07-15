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

// requirePytest skips a test-mode execution test where python3/pytest is
// unavailable, keeping `go test ./...` green on machines/CI without them (the
// containment proof is the on-target smoke, not the unit suite).
func requirePytest(t *testing.T) {
	t.Helper()
	requirePython(t)
	if err := exec.Command("python3", "-c", "import pytest").Run(); err != nil {
		t.Skip("pytest not importable; skipping test-runner mode test")
	}
}

// newPythonReportCap builds a Python runtime with a specific test-report cap, for
// the truncation test.
func newPythonReportCap(t *testing.T, cap int) *interpretedRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewPython(sb, 64*1024, 256, 64, cap)
}

// solutionFiles builds a files[] submission: one student module plus one hidden
// test file, mirroring how the backend assembles {student + hidden tests}.
func solutionFiles(testBody string) []runnerapi.RunFile {
	return []runnerapi.RunFile{
		{Path: "solution.py", Content: "def add(a, b):\n    return a + b\n"},
		{Path: "test_solution.py", Content: "from solution import add\n" + testBody},
	}
}

func testReq(files []runnerapi.RunFile) runnerapi.RunRequest {
	return runnerapi.RunRequest{Language: "python", Mode: "test", Files: files, TimeoutMs: 10000, MemoryMB: 256}
}

// runTestMode drives the runtime directly. The Service normally validates+clamps;
// these runtime-level tests set the limits themselves and exercise the executeTest
// path over the netns backend (no nsjail needed — the report hand-back works via
// cmd.Dir like nsjail's writable /sandbox bind).
func runTestMode(t *testing.T, rt *interpretedRuntime, req runnerapi.RunRequest) runnerapi.RunResult {
	t.Helper()
	return rt.Run(context.Background(), req)
}

func TestPythonTest_Pass(t *testing.T) {
	requirePytest(t)
	res := runTestMode(t, newPython(t), testReq(solutionFiles(
		"def test_a():\n    assert add(2, 3) == 5\ndef test_b():\n    assert add(0, 0) == 0\n")))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if res.ReportFormat != "junit-xml" {
		t.Fatalf("expected report_format junit-xml, got %q", res.ReportFormat)
	}
	if !strings.Contains(res.TestReport, "<testsuite") || !strings.Contains(res.TestReport, `tests="2"`) {
		t.Fatalf("report should be the junit xml with 2 tests, got: %s", res.TestReport)
	}
	if strings.Contains(res.TestReport, "failures=\"0\"") == false {
		t.Fatalf("all-pass report should record failures=0, got: %s", res.TestReport)
	}
}

// TestPythonTest_Fail pins the core G9 distinction: a suite that runs to
// completion with failing tests is tests_failed (framework exit 1 + a produced
// report), NOT runtime_error, and the raw report enumerates the failures for the
// backend to grade.
func TestPythonTest_Fail(t *testing.T) {
	requirePytest(t)
	res := runTestMode(t, newPython(t), testReq(solutionFiles(
		"def test_ok():\n    assert add(2, 3) == 5\n"+
			"def test_bad1():\n    assert add(2, 3) == 6\n"+
			"def test_bad2():\n    assert add(1, 1) == 3\n"+
			"def test_ok2():\n    assert add(1, 1) == 2\n")))
	if res.Status != runnerapi.StatusTestsFailed {
		t.Fatalf("expected tests_failed, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Fatalf("tests_failed must carry the framework's non-zero exit, got 0")
	}
	if !strings.Contains(res.TestReport, `failures="2"`) || !strings.Contains(res.TestReport, `tests="4"`) {
		t.Fatalf("report should record 2 failures of 4, got: %s", res.TestReport)
	}
}

// TestPythonTest_ImportErrorIsRuntimeError pins the crash-vs-fail line: a hidden
// test that fails to import (collection error, pytest exit 2) is a harness crash,
// classified runtime_error — distinct from tests_failed — because exit 2 is not
// the tests-failed code, even though pytest also emits a report for it.
func TestPythonTest_ImportErrorIsRuntimeError(t *testing.T) {
	requirePytest(t)
	res := runTestMode(t, newPython(t), testReq([]runnerapi.RunFile{
		{Path: "test_broken.py", Content: "import definitely_not_a_real_module_xyz\ndef test_x():\n    assert True\n"},
	}))
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("import/collection error must be runtime_error, got %q", res.Status)
	}
}

// TestPythonTest_CleanExitBeforeReportIsRuntimeError is the hostile-student
// regression: importing the submitted module can terminate pytest itself with a
// clean exit before collection or hidden tests run. Exit code 0 alone must never
// be promoted to success when the framework produced no completed report.
func TestPythonTest_CleanExitBeforeReportIsRuntimeError(t *testing.T) {
	requirePytest(t)
	res := runTestMode(t, newPython(t), testReq([]runnerapi.RunFile{
		{Path: "test_bypass.py", Content: "import os\nos._exit(0)\ndef test_must_run():\n    assert False\n"},
	}))
	if res.ExitCode != 0 {
		t.Fatalf("exploit fixture must terminate the harness cleanly, got exit %d", res.ExitCode)
	}
	if res.TestReport != "" {
		t.Fatalf("exploit fixture unexpectedly produced a report: %q", res.TestReport)
	}
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("clean exit without a report must fail closed as runtime_error, got %q", res.Status)
	}
}

var testcaseName = regexp.MustCompile(`<testcase[^>]*\bname="([^"]*)"`)

// TestPythonTest_DeterministicOrder pins the determinism guarantee at the level
// that matters for grading: the SAME suite over the SAME code discovers and runs
// its tests in an identical order across two independent runs (cache disabled, no
// order-shuffling plugin). JUnit embeds wall timestamps, so the whole report is
// not byte-identical — the test ORDER is the stable, grading-relevant invariant.
func TestPythonTest_DeterministicOrder(t *testing.T) {
	requirePytest(t)
	rt := newPython(t)
	req := testReq(solutionFiles(
		"def test_c():\n    assert add(1,1)==2\ndef test_a():\n    assert add(1,1)==2\ndef test_b():\n    assert add(1,1)==2\n"))
	a := runTestMode(t, rt, req)
	b := runTestMode(t, rt, req)
	if a.Status != runnerapi.StatusSuccess || b.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q / %q", a.Status, b.Status)
	}
	if oa, ob := testcaseName.FindAllString(a.TestReport, -1), testcaseName.FindAllString(b.TestReport, -1); strings.Join(oa, "|") != strings.Join(ob, "|") {
		t.Fatalf("test order not deterministic:\n  run1=%v\n  run2=%v", oa, ob)
	}
}

// TestPythonTest_ReportTruncated pins the report cap (RUNNER_MAX_TEST_REPORT_BYTES):
// a report larger than the cap is cut to the cap and the authoritative flag set.
func TestPythonTest_ReportTruncated(t *testing.T) {
	requirePytest(t)
	rt := newPythonReportCap(t, 128) // far below any real junit report
	res := runTestMode(t, rt, testReq(solutionFiles("def test_a():\n    assert add(2,3)==5\n")))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q", res.Status)
	}
	if !res.TestReportTruncated {
		t.Fatal("TestReportTruncated must be true when the report exceeds the cap")
	}
	if len(res.TestReport) != 128 {
		t.Fatalf("truncated report must equal the cap (128), got %d bytes", len(res.TestReport))
	}
}

// TestPythonTest_SingleSourceFile pins that test mode also accepts a single
// source_code submission (a self-contained test file), not only files[].
func TestPythonTest_SingleSourceFile(t *testing.T) {
	requirePytest(t)
	res := runTestMode(t, newPython(t), runnerapi.RunRequest{
		Language:   "python",
		Mode:       "test",
		SourceCode: "def test_self():\n    assert 1 + 1 == 2\n",
		TimeoutMs:  10000,
		MemoryMB:   256,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success on a self-contained test file, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if !strings.Contains(res.TestReport, `tests="1"`) {
		t.Fatalf("expected a 1-test report, got: %s", res.TestReport)
	}
}
