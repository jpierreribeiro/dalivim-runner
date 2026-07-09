package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// interpretedRuntime executes a single-file interpreted language in the sandbox.
// One instance serves exactly one language, configured by a languageSpec. It only
// describes WHAT to run (argv, workdir, limits) via a sandbox.Spec — the sandbox
// owns HOW it is contained (namespaces, rlimits, seccomp, process group, timeout
// kill), so this type holds no OS-level code and every language reuses the same
// containment. Adding an interpreted language is a languageSpec entry plus a
// constructor; this file does not change.
type interpretedRuntime struct {
	spec          languageSpec
	sandbox       sandbox.Sandbox
	bin           string // absolute interpreter path, resolved once (see resolveBin)
	version       string
	outputLimit   int
	maxProcesses  int // per-run RLIMIT_NPROC in the jail (fork-bomb cap)
	maxFileSizeMB int // per-run RLIMIT_FSIZE in the jail
}

// NewPython builds the Python runtime.
func NewPython(sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int) *interpretedRuntime {
	return newInterpreted(pythonSpec, sb, outputLimit, maxProcesses, maxFileSizeMB)
}

// NewNode builds the JavaScript (Node) runtime. It inherits the exact same jail
// as Python — the only differences (interpreter argv, source filename, version
// parsing) live in javascriptSpec, which is the whole point of the seam.
func NewNode(sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int) *interpretedRuntime {
	return newInterpreted(javascriptSpec, sb, outputLimit, maxProcesses, maxFileSizeMB)
}

// newInterpreted resolves the interpreter and detects its version once at
// construction so every result carries real provenance rather than a
// hand-configured value.
func newInterpreted(spec languageSpec, sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int) *interpretedRuntime {
	bin := resolveBin(spec.binNames)
	return &interpretedRuntime{
		spec:          spec,
		sandbox:       sb,
		bin:           bin,
		version:       detectVersion(bin, spec),
		outputLimit:   outputLimit,
		maxProcesses:  maxProcesses,
		maxFileSizeMB: maxFileSizeMB,
	}
}

func (r *interpretedRuntime) Language() string { return r.spec.name }
func (r *interpretedRuntime) Version() string  { return r.version }

// LimitFloors exposes the spec's per-language limit floors (G6); zero values
// mean the service's clamped limits are used as-is.
func (r *interpretedRuntime) LimitFloors() Floors {
	return Floors{TimeoutMs: r.spec.minTimeoutMs, MemoryMB: r.spec.minMemoryMB}
}

// Run writes the source to a throwaway directory and executes it under the
// sandbox: its own process group, an empty network namespace (when available), a
// wall-clock deadline, a CPU-seconds cap, and an address-space cap. Timeout and
// memory are already clamped by the Service.
func (r *interpretedRuntime) Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	workDir, err := os.MkdirTemp("", "dalivim-run-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create sandbox dir"}
	}
	defer os.RemoveAll(workDir)

	// Materialize the program and build the interpreter argv. Single-file
	// source_code takes the exact legacy path (one fixed-name file, spec.runArgs);
	// a files[] request materializes the validated tree under /sandbox/src and uses
	// the language's multi-file argv. The request was already validated by the
	// service, so a failure here is our own infrastructure fault (internal_error).
	runArgsTail, env, err := r.prepare(workDir, req)
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: err.Error()}
	}
	return r.execute(ctx, req, workDir, runArgsTail, env)
}

// RunBatch is the interpreted batch path (G6): the source is written (or the
// files[] tree materialized) once, then the interpreter is launched once per
// stdin, each in its own fresh jail. There is no artifact to amortize, so the
// saving is the repeated HTTP round-trip and source setup — smaller than the
// compiled win, kept for API symmetry.
func (r *interpretedRuntime) RunBatch(ctx context.Context, req runnerapi.RunRequest, totalBudgetMs int) runnerapi.BatchResult {
	start := time.Now()

	workDir, err := os.MkdirTemp("", "dalivim-run-*")
	if err != nil {
		return runnerapi.BatchResult{Status: runnerapi.StatusInternalError, Results: []runnerapi.RunResult{}}
	}
	defer os.RemoveAll(workDir)

	runArgsTail, env, err := r.prepare(workDir, req)
	if err != nil {
		return runnerapi.BatchResult{Status: runnerapi.StatusInternalError, Results: []runnerapi.RunResult{}}
	}

	results, aborted := batchLoop(ctx, req.Stdins, start, totalBudgetMs, func(ctx context.Context, stdin string) runnerapi.RunResult {
		rq := req
		rq.Stdin = stdin
		return r.execute(ctx, rq, workDir, runArgsTail, env)
	})
	return runnerapi.BatchResult{Status: runnerapi.BatchStatusOK, Results: results, Aborted: aborted}
}

// execute launches one interpreter process in the sandbox against the prepared
// workDir and classifies the outcome. It is a pure function of (req, workDir,
// runArgsTail, env): every call builds a fresh timeout ctx, output buffers, and
// jail, which is what lets RunBatch loop it safely.
func (r *interpretedRuntime) execute(ctx context.Context, req runnerapi.RunRequest, workDir string, runArgsTail, env []string) runnerapi.RunResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	// A second, cause-carrying cancel so an output flood can kill the child (via
	// cmd.Cancel → SIGKILL of the process group) and be told apart from a deadline
	// afterwards through context.Cause (G1.4).
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)

	// Build the jail argv: interpreter + any per-run memory flags + the run-args
	// tail (flags and the source FILENAME/entrypoint — never the code, so no shell
	// injection). Memory is enforced one of two ways depending on the runtime:
	// CPython gets a hard RLIMIT_AS (AddressSpaceMB); V8/Node can't take that cap
	// (its virtual cage), so it passes 0 and bounds its heap via memoryArgs.
	argv := []string{r.bin}
	if r.spec.memoryArgs != nil {
		argv = append(argv, r.spec.memoryArgs(req.MemoryMB)...)
	}
	argv = append(argv, runArgsTail...)

	addressSpaceMB := 0
	if r.spec.capAddressSpace {
		addressSpaceMB = req.MemoryMB
	}

	cmd, acct := r.sandbox.Command(ctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      req.TimeoutMs,
		AddressSpaceMB: addressSpaceMB,
		MemoryMB:       req.MemoryMB, // cgroup memory.max when a delegated cgroup is present
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
	})
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader(req.Stdin)
	cmd.Env = env

	// Both streams share one kill signal so a flood on either (stdout OR stderr)
	// stops the run; the child is SIGKILLed as soon as one crosses the cap.
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
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	// Classification order: a clean exit is success regardless of anything else;
	// otherwise the cgroup OOM event is the AUTHORITATIVE memory verdict (F-E/R6),
	// and the stderr substring is only the fallback for the no-cgroup path. An
	// output flood is checked before runtime_error but after timeout/memory so a
	// flood-then-deadline race resolves to whichever fired first (Cause reflects
	// the earlier cancel).
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case runErr == nil:
		res.Status = runnerapi.StatusSuccess
	case acct != nil && acct.OOMKilled():
		res.Status = runnerapi.StatusMemoryExceeded
	case r.spec.memErrSubstr != "" && strings.Contains(res.Stderr, r.spec.memErrSubstr):
		res.Status = runnerapi.StatusMemoryExceeded
	case errors.Is(context.Cause(ctx), errOutputLimit):
		res.Status = runnerapi.StatusOutputLimitExceeded
	default:
		res.Status = runnerapi.StatusRuntimeError
	}
	return res
}

// prepare materializes the submission into workDir and returns the interpreter
// run-args tail plus the child environment. The two shapes are kept separate on
// purpose:
//   - single-file (source_code): the ORIGINAL path — one fixed-name file at the
//     workdir root (/sandbox/<sourceFile>), spec.runArgs, spec.env. Byte-for-byte
//     unchanged from before G3.
//   - multi-file (files[]): the validated tree under workDir/src (/sandbox/src),
//     the language's multiFileRunArgs pointed at the entrypoint, and an env with
//     HOME set to a nonexistent path so no user config (.npmrc, .pythonrc) is read.
func (r *interpretedRuntime) prepare(workDir string, req runnerapi.RunRequest) (runArgs, env []string, err error) {
	if len(req.Files) == 0 {
		// 0600: only the uid that runs the interpreter needs to read the script.
		scriptPath := filepath.Join(workDir, r.spec.sourceFile)
		if werr := os.WriteFile(scriptPath, []byte(req.SourceCode), 0o600); werr != nil {
			return nil, nil, errors.New("could not write source")
		}
		return r.spec.runArgs, r.spec.env, nil
	}

	if r.spec.multiFileRunArgs == nil {
		return nil, nil, errors.New("language does not support multi-file submissions")
	}
	if _, merr := materializeSource(workDir, req); merr != nil {
		return nil, nil, merr
	}
	runArgs = r.spec.multiFileRunArgs(srcRel(req.Entrypoint))
	env = append(append([]string{}, r.spec.env...), "HOME=/nonexistent")
	return runArgs, env, nil
}

// memoryKB prefers the cgroup's authoritative peak (memory.peak) when a delegated
// cgroup accounted the run, falling back to the best-effort getrusage Maxrss.
func memoryKB(acct sandbox.RunAccounting, cmd *exec.Cmd) int {
	if acct != nil {
		if peak := acct.PeakMemoryKB(); peak > 0 {
			return peak
		}
	}
	return sandbox.MaxRSSkb(cmd)
}

// resolveBin resolves the first available interpreter to an ABSOLUTE path. The
// nsjail backend execve's argv[0] directly with no PATH search (the netns backend
// only got away with a bare name because it wraps argv in a shell), so a bare
// "python3"/"node" fails inside the jail with ENOENT. Resolving here keeps the
// runtime image-agnostic; the path is valid inside the jail because nsjail
// bind-mounts the same rootfs read-only. Falls back to the first candidate name
// if none is on PATH (e.g. off-Linux dev without the interpreter) rather than
// blocking boot.
func resolveBin(candidates []string) string {
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// detectVersion runs the spec's version command against the resolved interpreter
// once at construction. Empty string when the interpreter is absent (off-Linux
// dev) so provenance degrades gracefully instead of blocking boot.
func detectVersion(bin string, spec languageSpec) string {
	if bin == "" || len(spec.versionArgs) == 0 || spec.parseVersion == nil {
		return ""
	}
	out, err := exec.Command(bin, spec.versionArgs...).CombinedOutput()
	if err != nil {
		return ""
	}
	return spec.parseVersion(string(out))
}
