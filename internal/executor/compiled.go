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
}

var cppSpec = compiledLangSpec{
	name:       "cpp",
	sourceFile: "main.cpp",
	// No explicit link libs: the g++ driver folds -lm in and links libstdc++.
	compile:  []string{"g++", "-O2", "-static", "-std=c++20", "-o", "{out}", "{src}"},
	run:      []string{"{out}"},
	binNames: []string{"g++"},
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
	return &compiledRuntime{
		spec:             spec,
		sandbox:          sb,
		compilerBin:      bin,
		version:          detectVersion(bin, languageSpec{versionArgs: []string{"-dumpfullversion"}, parseVersion: strings.TrimSpace}),
		outputLimit:      cfg.OutputLimit,
		maxProcesses:     cfg.MaxProcesses,
		maxFileSizeMB:    cfg.MaxFileSizeMB,
		compileMemoryMB:  cfg.CompileMemoryMB,
		maxArtifactBytes: cfg.MaxArtifactBytes,
		runSeccomp:       cfg.RunSeccomp,
	}
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
	argv[0] = r.compilerBin // absolute path; nsjail does no PATH search

	cmd, acct := r.sandbox.Command(cctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      timeout,
		AddressSpaceMB: r.compileMemoryMB,
		MemoryMB:       r.compileMemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		Writable:       true, // the compiler writes its artifact into /sandbox
		MinimalRootfs:  false,
	})
	if acct != nil {
		defer acct.Close()
	}
	// TMPDIR so gcc's intermediates land in the size-capped tmpfs /tmp.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp"}
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

	cmd, acct := r.sandbox.Command(rctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      req.TimeoutMs,
		AddressSpaceMB: req.MemoryMB, // a static binary tolerates a hard RLIMIT_AS
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
	// A static artifact needs no environment, except: disable glibc's rseq
	// registration so __libc_start_main doesn't issue the rseq syscall — nsjail
	// 3.4's kafel cannot name rseq for the static seccomp allowlist, and rseq is a
	// pure perf optimisation, so turning it off costs nothing and keeps the
	// allowlist tight. Harmless under the denylist too.
	cmd.Env = []string{"GLIBC_TUNABLES=glibc.pthread.rseq=0"}

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
