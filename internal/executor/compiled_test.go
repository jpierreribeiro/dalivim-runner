package executor

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSubst pins the {src}/{out} placeholder substitution the compiled argv
// depends on — only whole placeholder tokens are replaced, order preserved.
func TestSubst(t *testing.T) {
	got := subst([]string{"gcc", "-O2", "-static", "-o", "{out}", "{src}"},
		"/sandbox/main.c", "/sandbox/bin")
	want := []string{"gcc", "-O2", "-static", "-o", "/sandbox/bin", "/sandbox/main.c"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("subst = %v, want %v", got, want)
	}
	// The run argv has only {out}.
	if r := subst([]string{"{out}"}, "", "/sandbox/bin"); r[0] != "/sandbox/bin" {
		t.Fatalf("run subst = %v", r)
	}
}

// TestCompiledSpecs pins the closed registry's shape: each compiled language has
// a compile step with both placeholders and a run step referencing the artifact.
func TestCompiledSpecs(t *testing.T) {
	for _, s := range []compiledLangSpec{cSpec, cppSpec} {
		joinedCompile := strings.Join(s.compile, " ")
		if !strings.Contains(joinedCompile, "{src}") || !strings.Contains(joinedCompile, "{out}") {
			t.Fatalf("%s compile must template {src} and {out}: %v", s.name, s.compile)
		}
		if !strings.Contains(joinedCompile, "-static") {
			t.Fatalf("%s must link -static so the run jail needs no libc: %v", s.name, s.compile)
		}
		if strings.Join(s.run, " ") != "{out}" {
			t.Fatalf("%s run must be exactly the artifact: %v", s.name, s.run)
		}
		if s.sourceFile == "" || len(s.binNames) == 0 {
			t.Fatalf("%s spec incomplete", s.name)
		}
	}
}

// TestCompiledLinkFlags pins G1.1: C links libm and the flag is rendered AFTER
// {src} so gcc's left-to-right symbol resolution finds it; C++ needs no explicit
// link libs (the g++ driver handles them).
func TestCompiledLinkFlags(t *testing.T) {
	if len(cSpec.link) != 1 || cSpec.link[0] != "-lm" {
		t.Fatalf("c must link -lm, got %v", cSpec.link)
	}
	if len(cppSpec.link) != 0 {
		t.Fatalf("cpp must not add explicit link libs, got %v", cppSpec.link)
	}
	// Mirror the compile()-time render: compile argv + link, then substitute.
	argv := subst(append(append([]string{}, cSpec.compile...), cSpec.link...), "/sandbox/main.c", "/sandbox/bin")
	joined := strings.Join(argv, " ")
	want := "gcc -O2 -static -o /sandbox/bin /sandbox/main.c -lm"
	if joined != want {
		t.Fatalf("c link render = %q, want %q", joined, want)
	}
	// -lm must come strictly after the source object.
	if strings.Index(joined, "-lm") < strings.Index(joined, "/sandbox/main.c") {
		t.Fatalf("-lm must follow {src} for link-order correctness: %q", joined)
	}
}

// TestSignalName maps a killed process to its SIGxxx name and a clean exit to "".
func TestSignalName(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	killed := exec.Command("sh", "-c", "kill -KILL $$")
	_ = killed.Run()
	if got := signalName(killed); got != "SIGKILL" {
		t.Fatalf("signalName(kill -KILL) = %q, want SIGKILL", got)
	}

	ok := exec.Command("sh", "-c", "exit 0")
	_ = ok.Run()
	if got := signalName(ok); got != "" {
		t.Fatalf("signalName(clean exit) = %q, want empty", got)
	}
}
