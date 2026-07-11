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

// requireSQLite skips execution tests where the sqlite3 CLI is unavailable,
// keeping `go test ./...` green on machines without it (the containment proof is
// the on-target smoke, not the unit suite).
func requireSQLite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not available; skipping SQL runner test")
	}
}

// newSQL builds a SQL runtime on the netns "off" backend (no nsjail dependency),
// exactly like newPython.
func newSQL(t *testing.T) *interpretedRuntime {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewSQL(sb, 64*1024, 256, 64, 4_000_000)
}

func sqlReq(script string) runnerapi.RunRequest {
	return runnerapi.RunRequest{Language: "sql", SourceCode: script, TimeoutMs: 3000, MemoryMB: 128}
}

func TestSQL_Success(t *testing.T) {
	requireSQLite(t)
	script := "CREATE TABLE t(id INTEGER, name TEXT);\n" +
		"INSERT INTO t VALUES (1,'alice'),(2,'bob');\n" +
		"SELECT * FROM t ORDER BY id;\n"
	res := run(t, newSQL(t), sqlReq(script))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ResultFormat != "json-rows" {
		t.Fatalf("expected result_format json-rows, got %q", res.ResultFormat)
	}
	// Stdout is a JSON array of row objects — and NOTHING else. The .dbconfig echo
	// must have been swallowed, so the output starts with '[' (no stray config line).
	out := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(out, "[{") {
		t.Fatalf("stdout must be a JSON row array, got: %q", res.Stdout)
	}
	if !strings.Contains(out, `"name":"alice"`) || !strings.Contains(out, `"id":2`) {
		t.Fatalf("rows missing expected values: %s", out)
	}
	if strings.Contains(res.Stdout, "load_extension") {
		t.Fatalf("the .dbconfig echo leaked into stdout: %q", res.Stdout)
	}
}

// TestSQL_Deterministic pins the G10 determinism contract: the same script run
// twice yields byte-identical output (pinned mode, NULL/real rendering).
func TestSQL_Deterministic(t *testing.T) {
	requireSQLite(t)
	rt := newSQL(t)
	script := "CREATE TABLE t(a REAL, b TEXT);\nINSERT INTO t VALUES (9.5,'x'),(7.0,NULL);\nSELECT * FROM t;\n"
	a := run(t, rt, sqlReq(script))
	b := run(t, rt, sqlReq(script))
	if a.Status != runnerapi.StatusSuccess || b.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q / %q", a.Status, b.Status)
	}
	if a.Stdout != b.Stdout {
		t.Fatalf("SQL output not deterministic:\n  run1=%q\n  run2=%q", a.Stdout, b.Stdout)
	}
	// NULL renders as JSON null (documented format).
	if !strings.Contains(a.Stdout, `"b":null`) {
		t.Fatalf("NULL must render as json null, got: %s", a.Stdout)
	}
}

// TestSQL_Error pins that a SQL error is a runtime_error (the query failed), with
// the engine's message on stderr and -bail's non-zero exit.
func TestSQL_Error(t *testing.T) {
	requireSQLite(t)
	res := run(t, newSQL(t), sqlReq("SELECT * FROM does_not_exist;\n"))
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error for a SQL error, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on SQL error, got 0")
	}
	if !strings.Contains(res.Stderr, "no such table") {
		t.Fatalf("stderr should carry the SQL error, got: %q", res.Stderr)
	}
}

// TestSQL_ExtensionLoadDisabled proves run-time extension loading is disabled
// (defense in depth): load_extension() is refused rather than attempting to open
// a shared object.
func TestSQL_ExtensionLoadDisabled(t *testing.T) {
	requireSQLite(t)
	res := run(t, newSQL(t), sqlReq("SELECT load_extension('/tmp/evil.so');\n"))
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("expected runtime_error, got %q", res.Status)
	}
	if !strings.Contains(res.Stderr, "not authorized") {
		t.Fatalf("load_extension must be refused as 'not authorized', got stderr: %q", res.Stderr)
	}
}

func TestSQL_ReportsVersion(t *testing.T) {
	requireSQLite(t)
	rt := newSQL(t)
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(rt.Version()) {
		t.Fatalf("expected a version like 3.x, got %q", rt.Version())
	}
}

// TestSQL_MultiFileRejected pins the phase-1 scope: SQL is source_code-only, so a
// files[] sql request is a clean 400 (no sql file policy), not a runtime failure.
func TestSQL_MultiFileRejected(t *testing.T) {
	svc := NewService(Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		&stubRuntime{lang: "sql"})
	_, err := svc.Run(context.Background(), runnerapi.RunRequest{
		Language: "sql",
		Files:    []runnerapi.RunFile{{Path: "query.sql", Content: "SELECT 1;"}},
	})
	wantValidation(t, err, "unsupported_language")
}
