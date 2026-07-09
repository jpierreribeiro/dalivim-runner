package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// compiledLangSpec describes one COMPILED language (C, C++): a compile command
// and a run command, each a token vector with two placeholders — {src} (the
// source path inside the jail) and {out} (the artifact path inside the jail).
// Like the interpreted registry it is CLOSED BY DESIGN: a new compiled language
// is a deliberate entry here plus a constructor, never request-driven.
type compiledLangSpec struct {
	name       string   // wire identifier, e.g. "c"
	sourceFile string   // basename the source is written to, e.g. "main.c"
	compile    []string // {out}/{src}-templated compiler argv; runs in the compile jail
	link       []string // link libraries appended AFTER {src} (e.g. "-lm"); order matters
	run        []string // {out}-templated run argv; runs in the stricter run jail
	binNames   []string // compiler lookup candidates (for version provenance)

	// compileEnv is extra environment for the COMPILE jail, appended to the base
	// PATH+TMPDIR (e.g. Go's isolated GOCACHE/GOPATH under the size-capped /tmp).
	compileEnv []string
	// runEnv is the environment for the RUN jail (a static artifact needs almost
	// none; C/C++ disable glibc rseq for the seccomp allowlist).
	runEnv []string
	// capAddressSpace applies a hard RLIMIT_AS (== MemoryMB) to the run jail. True
	// for C/C++ (a static glibc binary tolerates it). FALSE for Go: the Go runtime
	// reserves a huge virtual arena at startup and dies under any RLIMIT_AS ("failed
	// to reserve page summary memory"), exactly like V8 — so Go is bounded by the
	// cgroup memory.max only, despite being a static binary.
	capAddressSpace bool
	// staticAllowlistOK is whether this language MAY run under the tight static
	// seccomp allowlist. True for C/C++. FALSE for Go: its scheduler needs clone and
	// a wider syscall set than the C allowlist names, so Go stays on the denylist
	// (still strong: ro-rootfs + empty netns + no_new_privs + cgroup).
	staticAllowlistOK bool

	// versionArgs/parseVersion detect the compiler version for provenance (gcc
	// -dumpfullversion vs `go version`); parseVersion turns the raw output into the
	// reported runtime_version.
	versionArgs  []string
	parseVersion func(string) string

	// compileTmpfsMB sizes the compile jail's /tmp when the toolchain needs more
	// scratch than nsjail's default (Go's GOCACHE). 0 keeps the default.
	compileTmpfsMB int

	// compileArgv0Absolute means compile[0] is already an absolute path to execve
	// (e.g. /bin/sh for Go's cache-seeding prelude), so the compile builder must NOT
	// replace it with the resolved compiler binary.
	compileArgv0Absolute bool
}

var cSpec = compiledLangSpec{
	name:       "c",
	sourceFile: "main.c",
	// -static: no dynamic loader at runtime, so the run jail can be a minimal
	// rootfs with no libc present — the artifact needs nothing but itself.
	compile: []string{"gcc", "-O2", "-static", "-o", "{out}", "{src}"},
	// -lm links libm so ordinary C using <math.h> (sqrt/pow/sin…) resolves instead
	// of failing as a bogus compile_error. It MUST come after {src}: gcc resolves
	// libraries left-to-right, so a lib before the object that needs it is dropped.
	// libm only — no networking/dynamic-loading libs (would break the static run
	// jail and its seccomp allowlist).
	link:     []string{"-lm"},
	run:      []string{"{out}"},
	binNames: []string{"gcc", "cc"},
	// Disable glibc's rseq registration so __libc_start_main doesn't issue the rseq
	// syscall — nsjail 3.4's kafel cannot name rseq for the static allowlist, and
	// rseq is a pure perf optimisation. Harmless under the denylist too.
	runEnv:            []string{"GLIBC_TUNABLES=glibc.pthread.rseq=0"},
	capAddressSpace:   true,
	staticAllowlistOK: true,
	versionArgs:       []string{"-dumpfullversion"},
	parseVersion:      strings.TrimSpace,
}

var cppSpec = compiledLangSpec{
	name:       "cpp",
	sourceFile: "main.cpp",
	// No explicit link libs: the g++ driver folds -lm in and links libstdc++.
	compile:           []string{"g++", "-O2", "-static", "-std=c++20", "-o", "{out}", "{src}"},
	run:               []string{"{out}"},
	binNames:          []string{"g++"},
	runEnv:            []string{"GLIBC_TUNABLES=glibc.pthread.rseq=0"},
	capAddressSpace:   true,
	staticAllowlistOK: true,
	versionArgs:       []string{"-dumpfullversion"},
	parseVersion:      strings.TrimSpace,
}

var goSpec = compiledLangSpec{
	name:       "go",
	sourceFile: "main.go",
	// `go build` of a single file needs no go.mod (module/multi-file is G3).
	// CGO_ENABLED=0 yields a pure-static binary (no libc link), so the run jail
	// stays minimal-rootfs like C/C++.
	//
	// Every run gets a FRESH tmpfs /tmp, so GOCACHE would be cold on every compile —
	// a cold single-file build recompiles the stdlib deps and takes ~14 s on one
	// CPU, blowing the compile budget. So the compile seeds a writable /tmp/gocache
	// from the image's pre-warmed read-only /opt/gocache (Dockerfile), which turns a
	// warm build into ~0.3 s. The copy runs in the jail via /bin/sh; the argv is
	// absolute so nsjail execve's it directly (compileArgv0Absolute).
	// cp -r (not -a): the jail-private uid can't preserve root ownership, and -a
	// would exit non-zero trying, breaking the && chain. -r copies content with the
	// files owned by the jail uid and the dirs writable, which is what go build needs.
	compile:  []string{"/bin/sh", "-c", "cp -r /opt/gocache /tmp/gocache && exec go build -o {out} {src}"},
	run:      []string{"{out}"},
	binNames: []string{"go"},
	compileEnv: []string{
		"CGO_ENABLED=0",
		"GOCACHE=/tmp/gocache", // seeded from /opt/gocache by the compile prelude
		"GOPATH=/tmp/gopath",
		"GOTOOLCHAIN=local", // no network toolchain fetch (the jail has no egress)
		"GOENV=off",         // no $HOME dependency the compile jail doesn't have
	},
	compileArgv0Absolute: true,
	// GOMAXPROCS=1 keeps the scheduler to one OS thread — fewer clone/thread churn
	// under the denylist and the per-run pids cap, plenty for judged programs.
	runEnv:            []string{"GOMAXPROCS=1"},
	capAddressSpace:   false, // Go dies under RLIMIT_AS; cgroup memory.max bounds it
	staticAllowlistOK: false, // Go's scheduler needs a wider syscall set → denylist
	versionArgs:       []string{"version"},
	parseVersion:      parseGoVersion,
	// The seeded GOCACHE (~40 MB) plus incremental build output lives on /tmp; give
	// the compile jail a roomy tmpfs so it doesn't hit "no space left".
	compileTmpfsMB: 256,
}

// parseGoVersion pulls the bare version out of `go version` output
// ("go version go1.26 linux/amd64" → "1.26"), degrading to the trimmed raw
// string if the shape is unexpected.
func parseGoVersion(out string) string {
	for _, f := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(f, "go"); ok && len(v) > 0 && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return strings.TrimSpace(out)
}

// artifactName is the compiled binary's basename inside the per-run workdir; it
// is written by the compile jail and read (read-only) by the run jail.
const artifactName = "bin"

// compiledRuntime executes a compiled language in TWO separate jails: an
// untrusted compile jail (the compiler is itself hostile input — template/macro
// bombs, recursive includes) that writes a static artifact, then a stricter,
// minimal-rootfs run jail that executes only that artifact. It implements the
// same Runtime interface as interpreted languages, so dispatch never changes.
type compiledRuntime struct {
	spec             compiledLangSpec
	sandbox          sandbox.Sandbox
	compilerBin      string // absolute compiler path (nsjail execve's argv[0] directly)
	version          string
	outputLimit      int
	maxProcesses     int
	maxFileSizeMB    int
	compileMemoryMB  int                    // RLIMIT_AS/cgroup for the compile phase
	maxArtifactBytes int                    // reject artifacts larger than this (compile bombs)
	runSeccomp       sandbox.SeccompProfile // seccomp profile for the run jail
}

// CompiledConfig carries the compile-phase knobs (execution knobs are shared with
// interpreted runtimes and passed positionally). The compile timeout is NOT here:
// like the run timeout it is clamped in the service layer and arrives on the
// request (req.CompileTimeoutMs), so there is one place limits are enforced.
type CompiledConfig struct {
	OutputLimit      int
	MaxProcesses     int
	MaxFileSizeMB    int
	CompileMemoryMB  int
	MaxArtifactBytes int
	RunSeccomp       sandbox.SeccompProfile
}

// NewC / NewCpp build the C and C++ runtimes on the shared sandbox.
func NewC(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(cSpec, sb, cfg)
}
func NewCpp(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(cppSpec, sb, cfg)
}

func newCompiled(spec compiledLangSpec, sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	bin := resolveBin(spec.binNames)
	// A language that cannot take the tight static allowlist is pinned to the
	// denylist regardless of RUNNER_STATIC_SECCOMP, so enabling the allowlist for
	// C/C++ never SIGSYS-kills a Go run for a syscall the C set omits.
	runSeccomp := cfg.RunSeccomp
	if !spec.staticAllowlistOK {
		runSeccomp = sandbox.SeccompDenylist
	}
	return &compiledRuntime{
		spec:             spec,
		sandbox:          sb,
		compilerBin:      bin,
		version:          detectVersion(bin, languageSpec{versionArgs: spec.versionArgs, parseVersion: spec.parseVersion}),
		outputLimit:      cfg.OutputLimit,
		maxProcesses:     cfg.MaxProcesses,
		maxFileSizeMB:    cfg.MaxFileSizeMB,
		compileMemoryMB:  cfg.CompileMemoryMB,
		maxArtifactBytes: cfg.MaxArtifactBytes,
		runSeccomp:       runSeccomp,
	}
}

// NewGo builds the Go runtime. It reuses the two-jail compiled flow but runs on
// the denylist (not the static allowlist) and without RLIMIT_AS — see goSpec.
func NewGo(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(goSpec, sb, cfg)
}

func (r *compiledRuntime) Language() string { return r.spec.name }
func (r *compiledRuntime) Version() string  { return r.version }

// Run performs compile→run. A compile failure (non-zero exit or compile timeout)
// short-circuits to compile_error WITHOUT executing anything; only a validated
// artifact reaches the run jail.
func (r *compiledRuntime) Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	workDir, err := os.MkdirTemp("", "dalivim-build-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create build dir"}
	}
	defer os.RemoveAll(workDir)

	if err := os.WriteFile(filepath.Join(workDir, r.spec.sourceFile), []byte(req.SourceCode), 0o600); err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not write source"}
	}

	if res, ok := r.compile(ctx, req, workDir); !ok {
		return res // compile_error or internal_error — nothing was executed
	}
	return r.execute(ctx, req, workDir)
}

// compile runs the compile jail. It returns (result, false) to short-circuit on
// compile_error/internal_error, or (zero, true) when a valid artifact exists.
func (r *compiledRuntime) compile(ctx context.Context, req runnerapi.RunRequest, workDir string) (runnerapi.RunResult, bool) {
	// req.CompileTimeoutMs is already clamped to [default, ceiling] by the service
	// (mirrors req.TimeoutMs for the run phase).
	timeout := req.CompileTimeoutMs
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	srcJail := sandbox.JailPath(r.spec.sourceFile)
	outJail := sandbox.JailPath(artifactName)
	// Link libraries go AFTER {src} on the line so left-to-right symbol resolution
	// works regardless of which libs a future language adds (see spec.link).
	argv := subst(append(append([]string{}, r.spec.compile...), r.spec.link...), srcJail, outJail)
	if !r.spec.compileArgv0Absolute {
		argv[0] = r.compilerBin // absolute compiler path; nsjail does no PATH search
	}

	// The Go toolchain (`go build`) is itself a Go program and dies under any
	// RLIMIT_AS the same way its output does, so the compile jail skips the
	// address-space cap for a language that opts out and leans on the cgroup
	// memory.max (compileMemoryMB) instead.
	compileAS := 0
	if r.spec.capAddressSpace {
		compileAS = r.compileMemoryMB
	}
	cmd, acct := r.sandbox.Command(cctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      timeout,
		AddressSpaceMB: compileAS,
		MemoryMB:       r.compileMemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		TmpfsSizeMB:    r.spec.compileTmpfsMB,
		Writable:       true, // the compiler writes its artifact into /sandbox
		MinimalRootfs:  false,
	})
	if acct != nil {
		defer acct.Close()
	}
	// PATH includes the Go toolchain dir; TMPDIR so gcc/go intermediates land in
	// the size-capped tmpfs /tmp. Per-language compileEnv adds e.g. Go's isolated
	// GOCACHE/GOPATH.
	cmd.Env = append([]string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp"}, r.spec.compileEnv...)
	stderr := &limitedBuffer{limit: r.outputLimit}
	cmd.Stderr = stderr
	cmd.Stdout = &limitedBuffer{limit: r.outputLimit}

	runErr := cmd.Run()

	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: stderr.String() + "\n[compile timed out]",
		}, false
	}
	if runErr != nil {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: stderr.String(),
		}, false
	}

	// Validate the artifact: it must exist and stay under the size cap (a compile
	// bomb that somehow linked huge). Otherwise it is our failure, not the code's.
	fi, err := os.Stat(filepath.Join(workDir, artifactName))
	if err != nil || fi.Size() == 0 {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compile produced no artifact"}, false
	}
	if r.maxArtifactBytes > 0 && fi.Size() > int64(r.maxArtifactBytes) {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compiled artifact exceeds size cap"}, false
	}
	return runnerapi.RunResult{}, true
}

// execute runs the validated static artifact in the stricter, minimal-rootfs run
// jail (no toolchain, no libs) and classifies the outcome.
func (r *compiledRuntime) execute(ctx context.Context, req runnerapi.RunRequest, workDir string) runnerapi.RunResult {
	rctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	// Cause-carrying cancel so an output flood kills the artifact and is told apart
	// from a deadline afterwards through context.Cause (G1.4).
	rctx, cancelCause := context.WithCancelCause(rctx)
	defer cancelCause(nil)

	argv := subst(r.spec.run, "", sandbox.JailPath(artifactName))

	// A glibc static binary (C/C++) tolerates a hard RLIMIT_AS; the Go runtime does
	// not (it reserves a huge virtual arena and dies), so Go opts out and is bounded
	// by the cgroup memory.max only — see goSpec.capAddressSpace.
	runAS := 0
	if r.spec.capAddressSpace {
		runAS = req.MemoryMB
	}
	cmd, acct := r.sandbox.Command(rctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      req.TimeoutMs,
		AddressSpaceMB: runAS,
		MemoryMB:       req.MemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		Writable:       false,
		MinimalRootfs:  true,         // no toolchain in the run jail (D-4)
		Seccomp:        r.runSeccomp, // tight static allowlist when enabled (default: denylist)
	})
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader(req.Stdin)
	// The run env is per-language: C/C++ disable glibc rseq (see runEnv); Go pins
	// GOMAXPROCS. A static artifact otherwise needs no environment.
	cmd.Env = r.spec.runEnv

	onFlood := func() { cancelCause(errOutputLimit) }
	stdout := &limitedBuffer{limit: r.outputLimit, onLimit: onFlood}
	stderr := &limitedBuffer{limit: r.outputLimit, onLimit: onFlood}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := int(time.Since(start).Milliseconds())

	res := runnerapi.RunResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: duration,
		MemoryKB:   memoryKB(acct, cmd),
		Signal:     signalName(cmd),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case runErr == nil:
		res.Status = runnerapi.StatusSuccess
	case acct != nil && acct.OOMKilled():
		res.Status = runnerapi.StatusMemoryExceeded
	case errors.Is(context.Cause(rctx), errOutputLimit):
		res.Status = runnerapi.StatusOutputLimitExceeded
	default:
		res.Status = runnerapi.StatusRuntimeError
	}
	return res
}

// subst returns a copy of argv with each {src}/{out} placeholder token replaced.
func subst(argv []string, src, out string) []string {
	repl := strings.NewReplacer("{src}", src, "{out}", out)
	cp := make([]string, len(argv))
	for i, a := range argv {
		cp[i] = repl.Replace(a)
	}
	return cp
}

// signalName returns the "SIGxxx" name of the signal that killed the process, or
// "" when it exited normally. Disambiguates SIGSEGV crashes, SIGKILL (OOM/limit)
// and SIGSYS (seccomp) from a clean non-zero exit.
func signalName(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return ""
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	if name, known := signalNames[ws.Signal()]; known {
		return name
	}
	return "signal " + strconv.Itoa(int(ws.Signal()))
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGFPE:  "SIGFPE",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGSYS:  "SIGSYS",
	syscall.SIGILL:  "SIGILL",
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGXCPU: "SIGXCPU",
	syscall.SIGXFSZ: "SIGXFSZ",
}
