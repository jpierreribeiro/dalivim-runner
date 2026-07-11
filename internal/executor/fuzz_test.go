package executor

// S1 — fuzz the request validators (see docs/future/security/S1-fuzzing-validators.md).
//
// These targets assert CONTAINMENT INVARIANTS ("no accepted input escapes the
// source root", "an accepted Java class is never a path"), not expected outputs.
// A fuzzer turns the validators' universal claim ("NO input escapes") from a
// sampled property (the example tables) into a searched one. Every target is a
// pure/bounded function of its input so it can never become a CI resource sink;
// FuzzMaterialize additionally caps file count and content size and writes only
// under a per-iteration t.TempDir().
//
// Any crasher `go test` writes to testdata/fuzz/ is committed as a permanent
// regression seed, and — being a rejection the validator wrongly accepted — must
// be fixed fail-closed before the gate goes green again.

import (
	"encoding/base64"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// FuzzValidatePath is the load-bearing target: no path the validator ACCEPTS may
// ever escape the source root. Rejection is always acceptable (the validator is
// fail-closed by design); an acceptance that violates any invariant below is a
// host-filesystem arbitrary-write and must fail the fuzz run.
func FuzzValidatePath(f *testing.F) {
	seeds := []string{
		"main.py", "src/util.c", "../etc/passwd", "/abs", "a/../b", "a//b",
		".hidden", "-flag", "@argfile", "a\x00b", "café.py", "C:\\x",
		"..", ".", "a/..", "a/b/../../../etc/passwd", "__init__.py",
		"a/./b", "a/", "/", "", "a\\b", "a b.py", "\n.py", "a.PY",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	p, ok := policyFor("python", testCaps)
	if !ok {
		f.Fatal("no policy for python")
	}
	f.Fuzz(func(t *testing.T, raw string) {
		clean, err := validatePath(raw, p)
		if err != nil {
			return // rejection is always fine — the validator fails CLOSED
		}
		// ACCEPTED ⇒ every one of these must hold, or containment is broken.
		if path.IsAbs(clean) {
			t.Fatalf("accepted absolute path %q (from %q)", clean, raw)
		}
		if clean != path.Clean(clean) {
			t.Fatalf("accepted non-normal path %q (from %q)", clean, raw)
		}
		for _, c := range strings.Split(clean, "/") {
			if c == "" || c == "." || c == ".." {
				t.Fatalf("accepted traversal/empty component %q in %q (from %q)", c, clean, raw)
			}
		}
		// The killer property: joined under any root, the result stays strictly
		// under that root. filepath.Clean collapses any residual traversal; if the
		// prefix does not survive, the path escaped.
		const root = "/job"
		joined := filepath.Clean(filepath.Join(root, filepath.FromSlash(clean)))
		if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Fatalf("accepted path escapes root: %q -> %q -> %q", raw, clean, joined)
		}
	})
}

// FuzzResolveEntrypoint fuzzes the path-based entrypoint resolution: any entry the
// resolver ACCEPTS must be one of the submitted files (never a fabricated path) and
// must itself pass the path grammar. The submitted set includes the default entry
// so an empty input (which resolves to the default) is a legitimate acceptance.
func FuzzResolveEntrypoint(f *testing.F) {
	seeds := []string{"", "main.py", "helper.py", "../etc/passwd", "sub/mod.py", "nope.py", "-x", "a\x00b"}
	for _, s := range seeds {
		f.Add(s)
	}
	p, ok := policyFor("python", testCaps)
	if !ok {
		f.Fatal("no policy for python")
	}
	files := map[string]struct{}{"main.py": {}, "helper.py": {}, "sub/mod.py": {}}
	f.Fuzz(func(t *testing.T, entry string) {
		got, err := resolveEntrypoint(entry, files, p)
		if err != nil {
			return // rejection is fine
		}
		// ACCEPTED ⇒ it is a real submitted file, in normal form, under root.
		if _, ok := files[got]; !ok {
			t.Fatalf("resolved entrypoint %q (from %q) is not among the submitted files", got, entry)
		}
		if got != path.Clean(got) || path.IsAbs(got) {
			t.Fatalf("resolved entrypoint %q is not a clean relative path (from %q)", got, entry)
		}
	})
}

// FuzzJavaClass fuzzes the Java entrypoint resolver: an ACCEPTED class name is a
// class name, never a path — it must match javaClassRe and can contain no '/' and
// no ".." run, so it can never be smuggled through as a filesystem path.
func FuzzJavaClass(f *testing.F) {
	seeds := []string{"Main", "com.example.Main", "", "com/example/Main", "..", "com..Main", "1Bad", "$Weird", "a.b.c.D"}
	for _, s := range seeds {
		f.Add(s)
	}
	p, ok := policyFor("java", testCaps)
	if !ok {
		f.Fatal("no policy for java")
	}
	if !p.EntryIsClass {
		f.Fatal("java policy is expected to resolve a class entrypoint")
	}
	f.Fuzz(func(t *testing.T, entry string) {
		got, err := resolveEntrypoint(entry, nil, p)
		if err != nil {
			return // rejection is fine
		}
		if !javaClassRe.MatchString(got) {
			t.Fatalf("accepted Java entrypoint %q (from %q) does not match javaClassRe", got, entry)
		}
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Fatalf("accepted Java entrypoint %q looks like a path (from %q)", got, entry)
		}
	})
}

// FuzzBase64RoundTrip pins the binary-safe I/O identity: base64 encode/decode is a
// lossless round-trip for ANY byte string, and text mode is the identity. A break
// here means a program's raw output could be silently corrupted on the wire.
func FuzzBase64RoundTrip(f *testing.F) {
	seeds := []string{"", "hello", "4\n", "\x00\x01\x02\xff", "café", strings.Repeat("x", 300)}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// base64 mode must round-trip byte-for-byte.
		enc := encodeStream("base64", s)
		dec, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("encodeStream base64 output did not decode: %v (input %q)", err, s)
		}
		if string(dec) != s {
			t.Fatalf("base64 round-trip changed the payload: %q -> %q -> %q", s, enc, string(dec))
		}
		// text ("" / "utf8" / anything not base64) must be the identity.
		if got := encodeStream("utf8", s); got != s {
			t.Fatalf("text-mode encodeStream mutated the payload: %q -> %q", s, got)
		}
		if got := encodeStream("", s); got != s {
			t.Fatalf("default-mode encodeStream mutated the payload: %q -> %q", s, got)
		}
	})
}

// FuzzMaterialize is the end-to-end containment target: it fuzzes the validator AND
// the traversal-resistant writer together. For ANY small files[] payload, either
// the call errors (fail-closed) or every object created under the root is a regular
// file/dir whose path stays inside the root — no symlink, no escape. Count and
// content size are capped so the target can never write a large tree in CI.
func FuzzMaterialize(f *testing.F) {
	f.Add("main.py", "print(1)", "helper.py", "x = 2")
	f.Add("../escape.py", "x", "main.py", "y")
	f.Add("a/b/c.py", "x", "a/b/c.py", "y") // duplicate after normalization
	f.Add("main.py", strings.Repeat("z", 100), "", "")

	p, ok := policyFor("python", testCaps)
	if !ok {
		f.Fatal("no policy for python")
	}
	const maxContent = 4096 // keep the writer bounded regardless of fuzz input size
	f.Fuzz(func(t *testing.T, p1, c1, p2, c2 string) {
		if len(c1) > maxContent {
			c1 = c1[:maxContent]
		}
		if len(c2) > maxContent {
			c2 = c2[:maxContent]
		}
		files := []runnerapi.RunFile{{Path: p1, Content: c1}, {Path: p2, Content: c2}}

		root := t.TempDir()
		out, err := MaterializeFiles(root, files, p)
		if err != nil {
			return // rejection is always fine — fail-closed
		}
		// Success ⇒ every returned path resolves to a regular file strictly under
		// root, and the on-disk tree contains no symlink and nothing outside root.
		rootReal, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			t.Fatalf("evalsymlinks root: %v", rerr)
		}
		for _, mf := range out {
			if path.IsAbs(mf.Path) || mf.Path != path.Clean(mf.Path) {
				t.Fatalf("materialized path %q is not clean/relative", mf.Path)
			}
			abs := filepath.Join(rootReal, filepath.FromSlash(mf.Path))
			if abs != rootReal && !strings.HasPrefix(abs, rootReal+string(filepath.Separator)) {
				t.Fatalf("materialized path %q escapes root %q -> %q", mf.Path, rootReal, abs)
			}
		}
		assertContainedTree(t, rootReal)
	})
}

// assertContainedTree walks root and fails if any entry is a symlink/special file
// or resolves outside root — the on-disk half of the materialization containment
// claim, checked independently of the writer's own os.Root guarantees.
func assertContainedTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Fatalf("materialized tree contains a symlink: %q", p)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			t.Fatalf("materialized tree contains a non-regular file: %q (%v)", p, d.Type())
		}
		if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Fatalf("materialized tree walked outside root: %q not under %q", p, root)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk materialized tree: %v", err)
	}
}
