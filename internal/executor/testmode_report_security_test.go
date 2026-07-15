package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTestReportRejectsFinalSymlinkEscape(t *testing.T) {
	workDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "runner-secret")
	if err := os.WriteFile(outside, []byte("must-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "report.xml")); err != nil {
		t.Fatal(err)
	}

	report, produced, truncated := readTestReport(workDir, "report.xml", "", 1024)
	if report != "" || produced || truncated {
		t.Fatalf("symlinked report must fail closed, got report=%q produced=%v truncated=%v", report, produced, truncated)
	}
}

func TestReadTestReportRejectsSymlinkedParentEscape(t *testing.T) {
	workDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "report.xml"), []byte("must-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "reports")); err != nil {
		t.Fatal(err)
	}

	report, produced, truncated := readTestReport(workDir, "reports/report.xml", "", 1024)
	if report != "" || produced || truncated {
		t.Fatalf("report beneath symlinked parent must fail closed, got report=%q produced=%v truncated=%v", report, produced, truncated)
	}
}

func TestReadTestReportRejectsNonRegularFile(t *testing.T) {
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, "report.xml"), 0o700); err != nil {
		t.Fatal(err)
	}

	report, produced, truncated := readTestReport(workDir, "report.xml", "", 1024)
	if report != "" || produced || truncated {
		t.Fatalf("non-regular report must fail closed, got report=%q produced=%v truncated=%v", report, produced, truncated)
	}
}

func TestReadTestReportReadsOnlyThroughConfiguredCap(t *testing.T) {
	workDir := t.TempDir()
	want := strings.Repeat("x", 32)
	if err := os.WriteFile(filepath.Join(workDir, "report.xml"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	report, produced, truncated := readTestReport(workDir, "report.xml", "", 8)
	if report != want[:8] || !produced || !truncated {
		t.Fatalf("bounded report mismatch: report=%q produced=%v truncated=%v", report, produced, truncated)
	}
}
