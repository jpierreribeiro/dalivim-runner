package executor

import (
	"strings"
	"testing"
)

// TestOdinSpec pins the Odin runtime shape (B.1): a native single-file build via
// `odin build -file`, run as the artifact on the full-rootfs denylist jail. This
// is a SPEC-LEVEL test like the other compiled specs — the real jail compile+run
// is proven on-target by the CI Docker smoke, because the Odin toolchain is not
// present in the dev/unit-test image.
func TestOdinSpec(t *testing.T) {
	joined := strings.Join(odinSpec.compile, " ")
	if !strings.Contains(joined, "{src}") || !strings.Contains(joined, "{out}") {
		t.Fatalf("odin compile must template {src} and {out}: %v", odinSpec.compile)
	}
	// -file builds the single source rather than a package directory.
	if !strings.Contains(joined, "-file") {
		t.Fatalf("odin single-file build must pass -file: %v", odinSpec.compile)
	}
	if odinSpec.compile[0] != "odin" || odinSpec.compile[1] != "build" {
		t.Fatalf("odin compile must invoke `odin build`: %v", odinSpec.compile)
	}
	if strings.Join(odinSpec.run, " ") != "{out}" {
		t.Fatalf("odin run must be exactly the artifact: %v", odinSpec.run)
	}
	if odinSpec.sourceFile != "main.odin" {
		t.Fatalf("odin source file must be main.odin, got %q", odinSpec.sourceFile)
	}
	// A default odin build links libc dynamically → keep the full rootfs at run.
	if !odinSpec.runFullRootfs {
		t.Fatal("odin must keep the full rootfs (dynamic libc link) until a static build is verified")
	}
	// LLVM runtime syscall surface is wider than the C allowlist → denylist first.
	if odinSpec.staticAllowlistOK {
		t.Fatal("odin must stay on the denylist first (LLVM runtime syscalls)")
	}
	// Decided ON TARGET, and about the COMPILER rather than the program: the
	// artifact tolerates a hard RLIMIT_AS, but this flag gates both phases and
	// `odin build` embeds LLVM — under the compile jail's cap it panics with
	// "Out of Virtual Memory", which turned EVERY odin request into a
	// compile_error. Flipping this back re-breaks the language completely.
	if odinSpec.capAddressSpace {
		t.Fatal("odin must NOT cap address space: the LLVM-backed compiler panics under RLIMIT_AS (like `go build`)")
	}
	if len(odinSpec.binNames) == 0 || odinSpec.binNames[0] != "odin" {
		t.Fatalf("odin binNames must resolve `odin`: %v", odinSpec.binNames)
	}
	if odinSpec.parseVersion == nil {
		t.Fatal("odin must supply a version parser")
	}
}

func TestParseOdinVersion(t *testing.T) {
	cases := map[string]string{
		"odin version dev-2024-05:abc123": "dev-2024-05:abc123",
		"odin version 0.13.0":             "0.13.0",
		"dev-2024-05":                     "dev-2024-05", // no "version" word → last field
		"":                                "",
	}
	for in, want := range cases {
		if got := parseOdinVersion(in); got != want {
			t.Fatalf("parseOdinVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
