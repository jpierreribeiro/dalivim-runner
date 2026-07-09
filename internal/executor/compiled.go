package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// compiledSpec describes one COMPILED language. Unlike the interpreted registry,
// a compiled run has two phases in two separate jails (plan §3.4): a compile jail
// that builds a static artifact, and a stricter run jail that executes only that
// artifact with no toolchain present. The compiler is untrusted input too, so it
// is jailed as tightly as the program. Closed by design — adding a language is a
// deliberate entry here plus a constructor.
type compiledSpec struct {
	name          string   // wire identifier: "c", "cpp"
	sourceFile    string   // "main.c" — written 0600 in the workdir
	artifact      string   // "prog" — the compiled binary, relative to the workdir
	compilerNames []string // compiler lookup candidates; first on PATH wins
	// compileFlags are the flags between the compiler and `-o <artifact> <source>`.
	// -static avoids the dynamic loader so the run jail needs no /lib and the
	// binary runs with the toolchain absent.
	compileFlags []string
	versionArgs  []string
	parseVersion func(output string) string
}

var cSpec = compiledSpec{
	name:          "c",
	sourceFile:    "main.c",
	artifact:      "prog",
	compilerNames: []string{"gcc", "cc"},
	compileFlags:  []string{"-O2", "-static"},
	versionArgs:   []string{"--version"}, // "gcc (Debian 12.2.0-14) 12.2.0\n..."
	parseVersion:  compilerVersion,       // -> "12.2.0"
}

var cppSpec = compiledSpec{
	name:          "cpp",
	sourceFile:    "main.cpp",
	artifact:      "prog",
	compilerNames: []string{"g++"},
	compileFlags:  []string{"-O2", "-static", "-std=c++20"},
	versionArgs:   []string{"--version"},
	parseVersion:  compilerVersion,
}

// CompileLimits are the ceilings for the compile phase (distinct from the run
// phase, which uses the Service's run limits).
type CompileLimits struct {
	MemoryMB         int // RLIMIT_AS for the compiler (gcc tolerates a hard cap)
	DefaultTimeoutMs int // used when the request omits compile_timeout_ms
	MaxTimeoutMs     int // hard ceiling for the compile phase
	MaxArtifactBytes int // reject an artifact larger than this (internal_error)
}

// compiledRuntime builds and runs one compiled language. Both phases go through
// the same sandbox seam; the only difference is the compile jail binds the
// workdir writable (to emit the artifact) while the run jail binds it read-only.
type compiledRuntime struct {
	spec          compiledSpec
	sandbox       sandbox.Sandbox
	compilerBin   string
	version       string
	outputLimit   int
	maxProcesses  int
	maxFileSizeMB int
	compile       CompileLimits
}

// NewC builds the C runtime; NewCpp the C++ runtime. Both resolve their compiler
// and detect its version once at construction.
func NewC(sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int, cl CompileLimits) *compiledRuntime {
	return newCompiled(cSpec, sb, outputLimit, maxProcesses, maxFileSizeMB, cl)
}

func NewCpp(sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int, cl CompileLimits) *compiledRuntime {
	return newCompiled(cppSpec, sb, outputLimit, maxProcesses, maxFileSizeMB, cl)
}

func newCompiled(spec compiledSpec, sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int, cl CompileLimits) *compiledRuntime {
	bin := resolveBin(spec.compilerNames)
	return &compiledRuntime{
		spec:          spec,
		sandbox:       sb,
		compilerBin:   bin,
		version:       detectVersion(bin, languageSpec{versionArgs: spec.versionArgs, parseVersion: spec.parseVersion}),
		outputLimit:   outputLimit,
		maxProcesses:  maxProcesses,
		maxFileSizeMB: maxFileSizeMB,
		compile:       cl,
	}
}

func (r *compiledRuntime) Language() string { return r.spec.name }
func (r *compiledRuntime) Version() string  { return r.version }

// Run compiles the source in a writable compile jail, then executes the artifact
// in a separate read-only run jail. A compile failure (non-zero exit or compile
// timeout) is StatusCompileError with the compiler's stderr in CompileOutput;
// nothing is executed. The compiler's own diagnostics never mix with the
// program's runtime output.
func (r *compiledRuntime) Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	workDir, err := os.MkdirTemp("", "dalivim-run-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create sandbox dir"}
	}
	defer os.RemoveAll(workDir)

	// 0600: only the compiling uid needs to read the source.
	if err := os.WriteFile(filepath.Join(workDir, r.spec.sourceFile), []byte(req.SourceCode), 0o600); err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not write source"}
	}

	// ---- Phase 1: compile (writable jail; the compiler is untrusted too) ----
	compileTimeout := clamp(req.CompileTimeoutMs, r.compile.DefaultTimeoutMs, r.compile.MaxTimeoutMs)
	cctx, ccancel := context.WithTimeout(ctx, time.Duration(compileTimeout)*time.Millisecond)
	defer ccancel()

	// gcc -O2 -static -o prog main.c  — paths are workdir-relative (resolved by the
	// jail's cwd); the source is a FILE reference, never inlined.
	compileArgv := append([]string{r.compilerBin}, r.spec.compileFlags...)
	compileArgv = append(compileArgv, "-o", r.spec.artifact, r.spec.sourceFile)

	ccmd := r.sandbox.Command(cctx, sandbox.Spec{
		Argv:            compileArgv,
		WorkDir:         workDir,
		TimeoutMs:       compileTimeout,
		AddressSpaceMB:  r.compile.MemoryMB, // gcc tolerates a hard RLIMIT_AS (unlike V8)
		MaxProcesses:    r.maxProcesses,     // gcc forks cc1/as/ld — needs a few
		MaxFileSizeMB:   r.maxFileSizeMB,
		WritableWorkDir: true, // only /sandbox is writable; rootfs stays read-only
	})
	ccmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp"}
	compileOut := &limitedBuffer{limit: r.outputLimit}
	ccmd.Stdout = compileOut
	ccmd.Stderr = compileOut // gcc diagnostics go to stderr; merge for one CompileOutput
	compileErr := ccmd.Run()

	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		// A compile that blows the timeout (e.g. an explosive template) is a build
		// failure, not a run outcome — plan §4.4.
		return runnerapi.RunResult{
			Status:         runnerapi.StatusCompileError,
			CompileOutput:  appendNote(compileOut.String(), "compilation timed out"),
			RuntimeName:    r.spec.name,
			RuntimeVersion: r.version,
		}
	}
	if compileErr != nil {
		exit := 0
		if ccmd.ProcessState != nil {
			exit = ccmd.ProcessState.ExitCode()
		}
		return runnerapi.RunResult{
			Status:         runnerapi.StatusCompileError,
			CompileOutput:  compileOut.String(),
			ExitCode:       exit,
			RuntimeName:    r.spec.name,
			RuntimeVersion: r.version,
		}
	}

	// Validate the artifact exists and is within the size cap before executing it.
	artifactPath := filepath.Join(workDir, r.spec.artifact)
	info, err := os.Stat(artifactPath)
	if err != nil || info.Size() == 0 {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compiler produced no artifact", CompileOutput: compileOut.String()}
	}
	if r.compile.MaxArtifactBytes > 0 && info.Size() > int64(r.compile.MaxArtifactBytes) {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compiled artifact exceeds the size cap", CompileOutput: compileOut.String()}
	}

	// ---- Phase 2: run (read-only jail; no toolchain, run limits) ----
	rctx, rcancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer rcancel()

	rcmd := r.sandbox.Command(rctx, sandbox.Spec{
		Argv:            []string{"./" + r.spec.artifact}, // relative to the jail cwd
		WorkDir:         workDir,
		TimeoutMs:       req.TimeoutMs,
		AddressSpaceMB:  req.MemoryMB, // a native static binary tolerates the hard cap
		MaxProcesses:    r.maxProcesses,
		MaxFileSizeMB:   r.maxFileSizeMB,
		WritableWorkDir: false,
	})
	rcmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	rcmd.Stdin = strings.NewReader(req.Stdin)
	stdout := &limitedBuffer{limit: r.outputLimit}
	stderr := &limitedBuffer{limit: r.outputLimit}
	rcmd.Stdout = stdout
	rcmd.Stderr = stderr

	start := time.Now()
	runErr := rcmd.Run()
	duration := int(time.Since(start).Milliseconds())

	res := runnerapi.RunResult{
		Stdout:         stdout.String(),
		Stderr:         stderr.String(),
		CompileOutput:  compileOut.String(), // warnings, if any; empty on a clean build
		DurationMs:     duration,
		MemoryKB:       sandbox.MaxRSSkb(rcmd),
		Signal:         sandbox.TerminationSignal(rcmd),
		RuntimeName:    r.spec.name,
		RuntimeVersion: r.version,
	}
	if rcmd.ProcessState != nil {
		res.ExitCode = rcmd.ProcessState.ExitCode()
	}
	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case runErr == nil:
		res.Status = runnerapi.StatusSuccess
	default:
		// A native crash (SIGSEGV/SIGABRT), a non-zero exit, or an OOM under
		// RLIMIT_AS all land here as runtime_error; deterministic memory_exceeded
		// classification for compiled languages is F-E/F-F (R6).
		res.Status = runnerapi.StatusRuntimeError
	}
	return res
}

// compilerVersion extracts the trailing version token of a compiler's first
// `--version` line, e.g. "gcc (Debian 12.2.0-14) 12.2.0" -> "12.2.0".
func compilerVersion(out string) string {
	line := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		line = out[:i]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// appendNote appends a short reason to compiler output, keeping a separator only
// when there is prior text.
func appendNote(out, note string) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return note
	}
	return out + "\n" + note
}
