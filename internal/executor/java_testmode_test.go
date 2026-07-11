package executor

import (
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// TestJavaTestCommand_Spec pins the Java test-mode recipe (G9). Like the other
// compiled specs it is a spec-level test — the real jail execution is proven
// on-target in the CI Docker smoke, since javac, the JVM, and the bundled JUnit
// console jar (and the in-jail /sandbox layout the prelude uses) live only in the
// image. The recipe itself was verified end to end against a real JDK + JUnit
// console launcher during implementation.
func TestJavaTestCommand_Spec(t *testing.T) {
	tc, ok := compiledTestCommands["java"]
	if !ok {
		t.Fatal("java must be registered in compiledTestCommands")
	}
	joined := strings.Join(tc.argv, " ")
	// One jail: javac the whole tree (find handles nested packages) against the
	// bundled jar, then the JVM runs the console launcher over the classpath.
	for _, want := range []string{
		"javac", "-cp " + junitConsoleJar, "$(find /sandbox/src -name '*.java')",
		"|| exit 42", // the compile-failure sentinel
		"-jar " + junitConsoleJar, "execute", "--scan-class-path",
		"--reports-dir=/sandbox/reports", "-Xmx{mem}m",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("java test argv must contain %q: %v", want, tc.argv)
		}
	}
	if tc.reportFormat != "junit-xml" {
		t.Fatalf("java report_format must be junit-xml, got %q", tc.reportFormat)
	}
	// The JUnit XML is a FILE handed back over the writable /sandbox bind — not
	// stdout — so both the report file and the writable jail are load-bearing.
	if tc.reportFile == "" {
		t.Fatal("java must read its report from a file (JUnit XML), not stdout")
	}
	if !tc.writable {
		t.Fatal("java test jail must be writable for the classes + report hand-back")
	}
	if tc.compileFailExit != 42 {
		t.Fatalf("java compile-fail sentinel exit must be 42, got %d", tc.compileFailExit)
	}
	if tc.testsFailedExit != 1 {
		t.Fatalf("java tests-failed exit must be 1, got %d", tc.testsFailedExit)
	}
	// -Xmx (not RLIMIT_AS) bounds the JVM heap, and the run env must be explicit so
	// the child never inherits JAVA_TOOL_OPTIONS (which would inject flags via stderr).
	env := strings.Join(tc.env, " ")
	if !strings.Contains(env, "HOME=/nonexistent") {
		t.Fatalf("java test env must set a throwaway HOME, got %v", tc.env)
	}
	if strings.HasSuffix(tc.sourceFile, ".java") == false {
		t.Fatalf("java source_code test file must be a .java (public test class), got %q", tc.sourceFile)
	}
}

// TestJavaTestPolicy_AdmitsTestFiles pins that the Java TEST file policy admits a
// student class plus a hidden *Test.java in a nested package, and rejects a
// non-.java file — the on-disk-safety contract for a mode=test files[] submission.
func TestJavaTestPolicy_AdmitsTestFiles(t *testing.T) {
	p, ok := policyForMode("java", modeTest, FileCaps{MaxFiles: 50, MaxFileBytes: 262_144, MaxFilesBytes: 1_048_576, MaxPathBytes: 180, MaxPathDepth: 8})
	if !ok {
		t.Fatal("java must have a test file policy")
	}
	good := []runnerapi.RunFile{
		{Path: "com/acme/Solution.java", Content: "package com.acme;\npublic class Solution { public static int add(int a,int b){return a+b;} }\n"},
		{Path: "com/acme/SolutionTest.java", Content: "package com.acme;\nimport org.junit.jupiter.api.Test;\npublic class SolutionTest { @Test void t(){} }\n"},
	}
	if _, _, err := validatePaths(good, p); err != nil {
		t.Fatalf("valid java test tree rejected: %v", err)
	}
	bad := []runnerapi.RunFile{{Path: "pom.xml", Content: "<project/>"}}
	if _, _, err := validatePaths(bad, p); err == nil {
		t.Fatal("a non-.java file must be rejected by the java test policy")
	}
}
