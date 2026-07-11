package executor

import (
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// requireNode / newNode are shared with node_test.go (node's built-in --test
// runner needs no extra packages, so node on PATH is the only requirement).

// solutionFilesJS builds a files[] submission: one student module plus one hidden
// *.test.js discovered by node --test, mirroring {student + hidden tests}. The
// test requires the sibling student file by relative path — resolved on disk under
// the materialized src/ tree.
func solutionFilesJS(testBody string) []runnerapi.RunFile {
	return []runnerapi.RunFile{
		{Path: "solution.js", Content: "module.exports.add = (a, b) => a + b;\n"},
		{Path: "solution.test.js", Content: "const test = require('node:test');\n" +
			"const assert = require('node:assert');\n" +
			"const { add } = require('./solution.js');\n" + testBody},
	}
}

func jsTestReq(files []runnerapi.RunFile) runnerapi.RunRequest {
	return runnerapi.RunRequest{Language: "javascript", Mode: "test", Files: files, TimeoutMs: 10000, MemoryMB: 256}
}

func TestNodeTest_Pass(t *testing.T) {
	requireNode(t)
	res := runTestMode(t, newNode(t), jsTestReq(solutionFilesJS(
		"test('adds', () => assert.strictEqual(add(2, 3), 5));\n"+
			"test('zero', () => assert.strictEqual(add(0, 0), 0));\n")))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if res.ReportFormat != "tap13" {
		t.Fatalf("expected report_format tap13, got %q", res.ReportFormat)
	}
	if !strings.Contains(res.TestReport, "TAP version 13") || !strings.Contains(res.TestReport, "# pass 2") || !strings.Contains(res.TestReport, "# fail 0") {
		t.Fatalf("report should be TAP with 2 pass / 0 fail, got: %s", res.TestReport)
	}
}

// TestNodeTest_Fail pins the core G9 distinction for JS: a suite that runs with
// failing tests is tests_failed (node --test exit 1 + a produced TAP report), NOT
// runtime_error, and the raw report enumerates the failures for the backend.
func TestNodeTest_Fail(t *testing.T) {
	requireNode(t)
	res := runTestMode(t, newNode(t), jsTestReq(solutionFilesJS(
		"test('ok', () => assert.strictEqual(add(2, 3), 5));\n"+
			"test('bad', () => assert.strictEqual(add(2, 3), 6));\n")))
	if res.Status != runnerapi.StatusTestsFailed {
		t.Fatalf("expected tests_failed, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Fatalf("tests_failed must carry node's non-zero exit, got 0")
	}
	if !strings.Contains(res.TestReport, "not ok") || !strings.Contains(res.TestReport, "# fail 1") {
		t.Fatalf("report should record the failing test, got: %s", res.TestReport)
	}
}

// TestNodeTest_LoadErrorFoldsIntoReport pins the node-specific crash-vs-fail
// nuance: unlike pytest's distinct exit-2 collection error, node --test exits 1
// AND emits a TAP report when a test file throws while loading (a student module
// that fails to require), folding it into the report as a failing test. So the
// honest, dumb-runner relay is tests_failed with the load error in the raw TAP —
// NOT runtime_error. (A crash before any TAP is produced still falls through to
// runtime_error, covered by the report-produced guard.)
func TestNodeTest_LoadErrorFoldsIntoReport(t *testing.T) {
	requireNode(t)
	res := runTestMode(t, newNode(t), jsTestReq([]runnerapi.RunFile{
		{Path: "broken.test.js", Content: "require('./does-not-exist.js');\n" +
			"const test = require('node:test');\ntest('x', () => {});\n"},
	}))
	if res.Status != runnerapi.StatusTestsFailed {
		t.Fatalf("a JS test file that fails to load folds into the report as tests_failed, got %q", res.Status)
	}
	if res.TestReport == "" {
		t.Fatal("a load failure must still produce a TAP report (the authoritative 'ran' signal)")
	}
}

// TestNodeTest_SingleSourceFile pins that JS test mode also accepts a single
// self-contained source_code test (written to main.test.js so node discovers it).
func TestNodeTest_SingleSourceFile(t *testing.T) {
	requireNode(t)
	res := runTestMode(t, newNode(t), runnerapi.RunRequest{
		Language:   "javascript",
		Mode:       "test",
		SourceCode: "const test = require('node:test');\nconst assert = require('node:assert');\ntest('self', () => assert.ok(1 + 1 === 2));\n",
		TimeoutMs:  10000,
		MemoryMB:   256,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success on a self-contained test file, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if !strings.Contains(res.TestReport, "# pass 1") {
		t.Fatalf("expected a 1-pass TAP report, got: %s", res.TestReport)
	}
}
