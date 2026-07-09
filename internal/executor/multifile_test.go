package executor

import (
	"strings"
	"testing"
)

// mf builds the materialized-file list from paths (content irrelevant to argv).
func mf(paths ...string) []MaterializedFile {
	out := make([]MaterializedFile, len(paths))
	for i, p := range paths {
		out[i] = MaterializedFile{Path: p}
	}
	return out
}

// TestCompileUnits filters to compilation units (headers excluded) and sorts, so
// the compile argv is deterministic and never passes a header as a TU.
func TestCompileUnits(t *testing.T) {
	got := compileUnits(mf("util.h", "main.c", "util.c"), "c")
	want := []string{"/sandbox/src/main.c", "/sandbox/src/util.c"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("C compile units = %v, want %v", got, want)
	}
	// C++ compiles .cpp/.cc/.cxx; .hpp/.h are headers.
	got = compileUnits(mf("solver.hpp", "main.cpp", "solver.cpp"), "cpp")
	if strings.Join(got, " ") != "/sandbox/src/main.cpp /sandbox/src/solver.cpp" {
		t.Fatalf("C++ compile units = %v", got)
	}
}

// TestCFamilyMultiFile pins the C/C++ multi-file compile argv: all TUs, -static
// preserved from the base template, -I the source root for headers, link libs
// after the objects, artifact = the static binary.
func TestCFamilyMultiFile(t *testing.T) {
	argv := cSpec.multiFile.compileArgv(mf("util.h", "main.c", "util.c"), "main.c")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"gcc", "-static", "-o /sandbox/bin", "-I/sandbox/src", "/sandbox/src/main.c", "/sandbox/src/util.c", "-lm"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("C compile argv missing %q: %s", want, joined)
		}
	}
	// {src} must be gone (no leftover placeholder) and -lm after the objects.
	if strings.Contains(joined, "{src}") {
		t.Fatalf("unsubstituted {src} in %s", joined)
	}
	if strings.Index(joined, "-lm") < strings.Index(joined, "/sandbox/src/util.c") {
		t.Fatalf("-lm must follow the objects: %s", joined)
	}
	if cSpec.multiFile.artifactRel("main.c") != defaultArtifact {
		t.Fatalf("C artifact = %q", cSpec.multiFile.artifactRel("main.c"))
	}
	if got := strings.Join(cSpec.multiFile.runTemplate("main.c"), " "); got != "/sandbox/bin" {
		t.Fatalf("C run = %q", got)
	}
	// C++ carries -std=c++20 through from the base template.
	if !strings.Contains(strings.Join(cppSpec.multiFile.compileArgv(mf("main.cpp"), "main.cpp"), " "), "-std=c++20") {
		t.Fatal("C++ multi-file must keep -std=c++20 from the base spec")
	}
}

// TestGoMultiFile pins the Go multi-file build: a /bin/sh prelude that seeds the
// warm GOCACHE, cd's into the entrypoint's package dir (shell-quoted), and builds
// that package only — never ./...  — offline.
func TestGoMultiFile(t *testing.T) {
	argv := goSpec.multiFile.compileArgv(mf("main.go", "helper.go"), "main.go")
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" {
		t.Fatalf("go compile must be a /bin/sh -c prelude: %v", argv)
	}
	sh := argv[2]
	for _, want := range []string{"cp -r /opt/gocache /tmp/gocache", "cd '/sandbox/src'", "go build -trimpath -o /sandbox/bin ."} {
		if !strings.Contains(sh, want) {
			t.Fatalf("go prelude missing %q: %s", want, sh)
		}
	}
	if strings.Contains(sh, "./...") {
		t.Fatalf("go must build one package, not ./...: %s", sh)
	}
	// A subdir entrypoint builds that package directory.
	sub := goSpec.multiFile.compileArgv(mf("cmd/app/main.go"), "cmd/app/main.go")[2]
	if !strings.Contains(sub, "cd '/sandbox/src/cmd/app'") {
		t.Fatalf("go subdir entrypoint must build its package dir: %s", sub)
	}
	// Offline module policy is pinned on the spec.
	env := strings.Join(goSpec.multiFileCompileEnv, " ")
	for _, want := range []string{"GOPROXY=off", "GOSUMDB=off", "GOWORK=off"} {
		if !strings.Contains(env, want) {
			t.Fatalf("go multi-file env missing %q: %v", want, goSpec.multiFileCompileEnv)
		}
	}
}

// TestJavaMultiFile pins the Java multi-file build: javac all sources into
// /sandbox/classes, artifact path derived from the entry class (packages →
// subdirs), run the entry class off that classpath.
func TestJavaMultiFile(t *testing.T) {
	argv := javaSpec.multiFile.compileArgv(mf("Main.java", "Helper.java"), "Main")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"javac", "-d /sandbox/classes", "/sandbox/src/Helper.java", "/sandbox/src/Main.java"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("javac argv missing %q: %s", want, joined)
		}
	}
	if got := javaSpec.multiFile.artifactRel("Main"); got != "classes/Main.class" {
		t.Fatalf("java artifact(Main) = %q", got)
	}
	if got := javaSpec.multiFile.artifactRel("com.acme.Main"); got != "classes/com/acme/Main.class" {
		t.Fatalf("java artifact(com.acme.Main) = %q", got)
	}
	run := javaSpec.multiFile.runTemplate("com.acme.Main")
	joinedRun := strings.Join(run, " ")
	for _, want := range []string{"-Xmx{mem}m", "-cp /sandbox/classes", "com.acme.Main"} {
		if !strings.Contains(joinedRun, want) {
			t.Fatalf("java run template missing %q: %s", want, joinedRun)
		}
	}
}

// TestInterpretedMultiFileArgs pins the interpreted argv tails: Python's runpy
// wrapper with the source root (not a caller path) on sys.path, and Node's `--`
// stop-flag guard before the entrypoint.
func TestInterpretedMultiFileArgs(t *testing.T) {
	py := pythonSpec.multiFileRunArgs("src/main.py")
	if len(py) != 4 || py[0] != "-I" || py[1] != "-B" || py[2] != "-c" {
		t.Fatalf("python multi-file flags = %v", py)
	}
	if !strings.Contains(py[3], `sys.path.insert(0, "src")`) || !strings.Contains(py[3], `run_path("src/main.py"`) {
		t.Fatalf("python runpy wrapper wrong: %s", py[3])
	}
	js := javascriptSpec.multiFileRunArgs("src/main.js")
	if strings.Join(js, " ") != "--disable-proto=throw -- src/main.js" {
		t.Fatalf("node multi-file args = %v", js)
	}
}
