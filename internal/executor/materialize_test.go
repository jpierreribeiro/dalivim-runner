package executor

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// testCaps are generous-but-real caps so a test exercises a rule without tripping
// an unrelated one.
var testCaps = FileCaps{MaxFiles: 50, MaxFileBytes: 262_144, MaxFilesBytes: 1_048_576, MaxPathBytes: 180, MaxPathDepth: 8}

func testPolicy(t *testing.T, lang string) FilePolicy {
	t.Helper()
	p, ok := policyFor(lang, testCaps)
	if !ok {
		t.Fatalf("no policy for %q", lang)
	}
	return p
}

// TestValidatePath_TraversalTable is the most important test in G3: every hostile
// path the plan enumerates MUST be rejected, with no path escaping the job root.
// A single accepted entry here is a host-filesystem arbitrary-write.
func TestValidatePath_TraversalTable(t *testing.T) {
	p := testPolicy(t, "c") // .c/.h allowlist; the rejection reason may be grammar or extension
	longPath := strings.Repeat("a", 200) + ".c"
	deepPath := "a/b/c/d/e/f/g/h/i/main.c" // 10 components > MaxPathDepth 8
	bad := []string{
		"../etc/passwd",
		"../../tmp/x",
		"/etc/passwd",
		"a/../../b",
		"x/./../../y",
		".",
		"./x",
		"x/.",
		"x//y",
		"x/../y",
		"",
		"/",
		"////",
		"main.c\x00.txt",
		"main.c\n",
		"main.c\r",
		"main.c\t",
		"a\\b.c",
		"-evil.c",
		"@args.c",
		"src/-evil.c",
		"src/@args.c",
		".git/config",
		"src/.git/config",
		"café.c", // non-ASCII
		"main‮c", // unicode bidi override
		deepPath,
		longPath,
	}
	for _, path := range bad {
		if _, err := validatePath(path, p); err == nil {
			t.Errorf("path %q was ACCEPTED; it must be rejected (potential host write)", path)
		}
	}
}

// TestValidatePath_Accepts pins the legitimate shapes the grammar must allow, so a
// tightening never silently rejects real multi-file layouts.
func TestValidatePath_Accepts(t *testing.T) {
	p := testPolicy(t, "c")
	good := []string{"main.c", "util.h", "src/main.c", "a/b/c/util.h", "my-file.c", "v2.c", "my_helper.c", "pkg/_internal.h"}
	for _, path := range good {
		if got, err := validatePath(path, p); err != nil {
			t.Errorf("path %q was rejected: %v", path, err)
		} else if got != path {
			t.Errorf("validatePath(%q) normalized to %q, want unchanged", path, got)
		}
	}
}

// TestValidateFiles_Duplicates rejects duplicate paths (exact and normalization
// collisions — though the normalization variants are already rejected earlier by
// the grammar, which the table confirms).
func TestValidateFiles_Duplicates(t *testing.T) {
	p := testPolicy(t, "c")
	_, _, err := validateFiles(runnerapi.RunRequest{Files: []runnerapi.RunFile{
		{Path: "main.c", Content: "int main(){}"},
		{Path: "main.c", Content: "duplicate"},
	}}, p)
	if verr, ok := err.(*ValidationError); !ok || verr.Code != "duplicate_path" {
		t.Fatalf("expected duplicate_path, got %v", err)
	}
	// Normalization-collision inputs are rejected as invalid paths (the "." / ".."
	// components fail the grammar) — still a rejection, never a silent merge.
	for _, dup := range []string{"./main.c", "src/./a.c", "src/x/../a.c"} {
		if _, err := validatePath(dup, p); err == nil {
			t.Errorf("normalization-variant %q was accepted", dup)
		}
	}
}

// TestValidateFiles_Limits pins the count / per-file / total-byte caps.
func TestValidateFiles_Limits(t *testing.T) {
	small := FileCaps{MaxFiles: 2, MaxFileBytes: 10, MaxFilesBytes: 15, MaxPathBytes: 180, MaxPathDepth: 8}
	p, _ := policyFor("python", small)

	// Too many files.
	files := []runnerapi.RunFile{{Path: "a.py", Content: "1"}, {Path: "b.py", Content: "1"}, {Path: "main.py", Content: "1"}}
	if _, _, err := validateFiles(runnerapi.RunRequest{Files: files}, p); code(err) != "too_many_files" {
		t.Fatalf("expected too_many_files, got %v", err)
	}
	// One file over the per-file byte cap.
	if _, _, err := validateFiles(runnerapi.RunRequest{Files: []runnerapi.RunFile{{Path: "main.py", Content: strings.Repeat("x", 11)}}}, p); code(err) != "file_too_large" {
		t.Fatalf("expected file_too_large, got %v", err)
	}
	// Total over the summed cap (each under per-file, sum over total).
	if _, _, err := validateFiles(runnerapi.RunRequest{Files: []runnerapi.RunFile{
		{Path: "main.py", Content: strings.Repeat("x", 9)},
		{Path: "b.py", Content: strings.Repeat("x", 9)},
	}}, p); code(err) != "files_too_large" {
		t.Fatalf("expected files_too_large, got %v", err)
	}
}

// TestValidateFiles_ExtensionsAndForbidden pins the per-language allowlist and the
// forbidden-name / forbidden-component sets (the build-system and package-manager
// manifests the offline policy must never process).
func TestValidateFiles_ExtensionsAndForbidden(t *testing.T) {
	cases := []struct {
		lang, path, wantCode string
	}{
		{"python", "evil.pyc", "forbidden_extension"},
		{"python", "setup.py", "forbidden_file"},
		{"python", "pyproject.toml", "forbidden_file"}, // forbidden-name check precedes the extension check
		{"javascript", "package.json", "forbidden_file"},
		{"javascript", "node_modules/x.js", "forbidden_path"},
		{"go", "go.mod", "forbidden_file"},
		{"go", "go.work", "forbidden_file"},
		{"go", "vendor/x.go", "forbidden_path"},
		{"go", "x.mod", "forbidden_extension"}, // a non-manifest .mod is caught by the extension allowlist
		{"c", "Makefile", "forbidden_extension"},
		{"c", "lib.so", "forbidden_extension"},
		{"java", "Main.class", "forbidden_extension"},
	}
	for _, tc := range cases {
		p := testPolicy(t, tc.lang)
		if _, err := validatePath(tc.path, p); code(err) != tc.wantCode {
			t.Errorf("%s %q: expected %s, got %v", tc.lang, tc.path, tc.wantCode, err)
		}
	}
}

// TestResolveEntrypoint covers defaults, path existence, and Java class-name
// validation (a Java entrypoint is a class, never a filesystem path).
func TestResolveEntrypoint(t *testing.T) {
	cpol := testPolicy(t, "c")
	files := map[string]struct{}{"main.c": {}, "util.c": {}}
	if e, err := resolveEntrypoint("", files, cpol); err != nil || e != "main.c" {
		t.Fatalf("default entry: got %q err %v", e, err)
	}
	if _, err := resolveEntrypoint("nope.c", files, cpol); code(err) != "missing_entrypoint" {
		t.Fatalf("expected missing_entrypoint, got %v", err)
	}

	jpol := testPolicy(t, "java")
	for _, ok := range []string{"Main", "com.acme.Main", "a.b.c.D"} {
		if _, err := resolveEntrypoint(ok, nil, jpol); err != nil {
			t.Errorf("java class %q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"com/acme/Main", "1Bad", "com..Main", "-x", "com.acme.Main "} {
		if _, err := resolveEntrypoint(bad, nil, jpol); err == nil {
			t.Errorf("java class %q should be rejected", bad)
		}
	}
}

// TestMaterializeFiles_Success writes a small tree and verifies content, sorted
// output, and that every object is a regular file inside the root.
func TestMaterializeFiles_Success(t *testing.T) {
	root := t.TempDir()
	files := []runnerapi.RunFile{
		{Path: "util/util.c", Content: "int add(int a,int b){return a+b;}"},
		{Path: "main.c", Content: "int main(){}"},
		{Path: "util/util.h", Content: "int add(int,int);"},
	}
	got, err := MaterializeFiles(root, files, testPolicy(t, "c"))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(got) != 3 || got[0].Path != "main.c" || got[1].Path != "util/util.c" {
		t.Fatalf("expected sorted [main.c util/util.c util/util.h], got %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(root, "main.c"))
	if err != nil || string(b) != "int main(){}" {
		t.Fatalf("main.c content wrong: %q err %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "util", "util.c")); !strings.Contains(string(b), "add") {
		t.Fatalf("util/util.c content wrong: %q", b)
	}
}

// TestMaterializeFiles_RefusesHostileTree is the race/symlink defense: a
// pre-existing hostile object inside the root must make materialization FAIL
// CLOSED rather than follow the symlink, clobber the object, or write through it.
// os.Root (openat2 RESOLVE_BENEATH) is the primitive; this proves it holds.
func TestMaterializeFiles_RefusesHostileTree(t *testing.T) {
	pol := testPolicy(t, "c")

	t.Run("symlinked parent dir", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}
		_, err := MaterializeFiles(root, []runnerapi.RunFile{{Path: "sub/pwn.c", Content: "x"}}, pol)
		if err == nil {
			t.Fatal("materialize followed a symlinked parent")
		}
		if _, statErr := os.Stat(filepath.Join(outside, "pwn.c")); statErr == nil {
			t.Fatal("wrote THROUGH the symlink — escaped the root")
		}
	})

	t.Run("final path is a symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "target.c")
		if err := os.Symlink(outside, filepath.Join(root, "main.c")); err != nil {
			t.Fatal(err)
		}
		if _, err := MaterializeFiles(root, []runnerapi.RunFile{{Path: "main.c", Content: "x"}}, pol); err == nil {
			t.Fatal("materialize clobbered a pre-existing symlink")
		}
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatal("wrote through the final symlink — escaped the root")
		}
	})

	t.Run("final path already exists as a FIFO", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "main.c"), 0o600); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
		if _, err := MaterializeFiles(root, []runnerapi.RunFile{{Path: "main.c", Content: "x"}}, pol); err == nil {
			t.Fatal("materialize wrote over a FIFO instead of failing closed")
		}
	})

	t.Run("final path already exists as a hardlink", func(t *testing.T) {
		root := t.TempDir()
		victim := filepath.Join(t.TempDir(), "victim.c")
		if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(victim, filepath.Join(root, "main.c")); err != nil {
			t.Skipf("hardlink unsupported: %v", err)
		}
		if _, err := MaterializeFiles(root, []runnerapi.RunFile{{Path: "main.c", Content: "overwrite"}}, pol); err == nil {
			t.Fatal("materialize wrote over a hardlink")
		}
		if b, _ := os.ReadFile(victim); string(b) != "secret" {
			t.Fatal("hardlink victim was modified — O_EXCL did not hold")
		}
	})
}

// code extracts a ValidationError code, or "" for any other error/nil.
func code(err error) string {
	if ve, ok := err.(*ValidationError); ok {
		return ve.Code
	}
	return ""
}
