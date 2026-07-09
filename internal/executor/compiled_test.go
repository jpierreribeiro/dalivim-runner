package executor

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
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

// TestGoSpec pins G2.1's Go runtime shape: a static build with NO RLIMIT_AS (the
// Go runtime dies under one), forced onto the denylist (its scheduler needs a
// wider syscall set than the C allowlist), with an isolated build cache and a
// roomy compile /tmp.
func TestGoSpec(t *testing.T) {
	if goSpec.capAddressSpace {
		t.Fatal("go must NOT cap address space: the Go runtime dies under RLIMIT_AS")
	}
	if goSpec.staticAllowlistOK {
		t.Fatal("go must stay on the denylist: its scheduler needs clone/futex/etc.")
	}
	if strings.Join(goSpec.run, " ") != "{out}" {
		t.Fatalf("go run must be exactly the artifact: %v", goSpec.run)
	}
	if goSpec.compileTmpfsMB <= 64 {
		t.Fatalf("go compile /tmp must be roomy for GOCACHE, got %dMB", goSpec.compileTmpfsMB)
	}
	joinedEnv := strings.Join(goSpec.compileEnv, " ")
	for _, want := range []string{"CGO_ENABLED=0", "GOCACHE=/tmp/", "GOTOOLCHAIN=local"} {
		if !strings.Contains(joinedEnv, want) {
			t.Fatalf("go compileEnv must contain %q: %v", want, goSpec.compileEnv)
		}
	}
	// The compile seeds the fresh tmpfs GOCACHE from the image's pre-warmed cache
	// via an absolute-argv0 shell prelude (a cold build blows the compile budget).
	if !goSpec.compileArgv0Absolute || goSpec.compile[0] != "/bin/sh" {
		t.Fatalf("go compile must run via an absolute /bin/sh prelude: %v", goSpec.compile)
	}
	if !strings.Contains(strings.Join(goSpec.compile, " "), "/opt/gocache") {
		t.Fatalf("go compile must seed GOCACHE from the pre-warmed /opt/gocache: %v", goSpec.compile)
	}
}

// TestCompiledSeccompPerLanguage pins that a language which cannot take the static
// allowlist is pinned to the denylist even when the allowlist is enabled — so
// turning on RUNNER_STATIC_SECCOMP for C/C++ never SIGSYS-kills a Go run.
func TestCompiledSeccompPerLanguage(t *testing.T) {
	cfg := CompiledConfig{RunSeccomp: sandbox.SeccompStaticEnforce}
	if got := newCompiled(cSpec, nil, cfg).runSeccomp; got != sandbox.SeccompStaticEnforce {
		t.Fatalf("C must keep the enabled allowlist, got %v", got)
	}
	if got := newCompiled(goSpec, nil, cfg).runSeccomp; got != sandbox.SeccompDenylist {
		t.Fatalf("Go must be forced to the denylist regardless of config, got %v", got)
	}
}

// TestParseGoVersion pins the `go version` → bare-version parse.
func TestParseGoVersion(t *testing.T) {
	cases := map[string]string{
		"go version go1.26 linux/amd64":    "1.26",
		"go version go1.24.7 darwin/arm64": "1.24.7",
		"weird output":                     "weird output",
	}
	for in, want := range cases {
		if got := parseGoVersion(in); got != want {
			t.Fatalf("parseGoVersion(%q) = %q, want %q", in, got, want)
		}
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
