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

// PythonRuntime executes Python 3 source in the sandbox. It is the only runtime
// wired today; the Runtime interface exists so a second language is purely
// additive. It only describes WHAT to run (argv, workdir, limits) via a Spec —
// the sandbox owns HOW it is contained.
type PythonRuntime struct {
	sandbox       sandbox.Sandbox
	bin           string // absolute interpreter path
	version       string
	outputLimit   int
	maxProcesses  int // per-run RLIMIT_NPROC in the jail (fork-bomb cap)
	maxFileSizeMB int // per-run RLIMIT_FSIZE in the jail
}

// NewPython builds the Python runtime. It resolves the interpreter's absolute
// path and version once at construction: the nsjail backend execve's the argv
// directly (no PATH search), so a bare "python3" would fail with ENOENT — the
// absolute path works under both backends. outputLimit caps captured
// stdout/stderr; maxProcesses and maxFileSizeMB are the per-run jail caps.
func NewPython(sb sandbox.Sandbox, outputLimit, maxProcesses, maxFileSizeMB int) *PythonRuntime {
	return &PythonRuntime{
		sandbox:       sb,
		bin:           detectPythonBin(),
		version:       detectPythonVersion(),
		outputLimit:   outputLimit,
		maxProcesses:  maxProcesses,
		maxFileSizeMB: maxFileSizeMB,
	}
}

func (p *PythonRuntime) Language() string { return "python" }
func (p *PythonRuntime) Version() string  { return p.version }

// detectPythonBin resolves the interpreter's absolute path via PATH. The
// read-only rootfs the jail bind-mounts is the host's, so this path is valid
// inside the jail too. Falls back to the bare name (best effort) if not found.
func detectPythonBin() string {
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return "python3"
}

func detectPythonVersion() string {
	out, err := exec.Command("python3", "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out)) // "Python 3.12.3\n" -> "3.12.3"
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

// Run writes the source to a throwaway directory and executes it under the
// sandbox: its own process group, an empty network namespace (when available), a
// wall-clock deadline, a CPU-seconds cap, and an address-space cap. Timeout and
// memory are already clamped by the Service.
func (p *PythonRuntime) Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	workDir, err := os.MkdirTemp("", "dalivim-run-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create sandbox dir"}
	}
	defer os.RemoveAll(workDir)

	// 0600: only the uid that runs python needs to read the script.
	scriptPath := filepath.Join(workDir, "main.py")
	if err := os.WriteFile(scriptPath, []byte(req.SourceCode), 0o600); err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not write source"}
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()

	// Describe the run; the sandbox turns this into a fully contained command
	// (namespaces, rlimits, seccomp, process group, timeout kill). "main.py" is
	// resolved against the sandbox-set working directory; the source lives in that
	// file, never on the command line, so there is no shell injection.
	cmd := p.sandbox.Command(ctx, sandbox.Spec{
		Argv:          []string{p.bin, "-I", "main.py"},
		WorkDir:       workDir,
		TimeoutMs:     req.TimeoutMs,
		MemoryMB:      req.MemoryMB,
		MaxProcesses:  p.maxProcesses,
		MaxFileSizeMB: p.maxFileSizeMB,
	})
	cmd.Stdin = strings.NewReader(req.Stdin)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONUNBUFFERED=1"}

	stdout := &limitedBuffer{limit: p.outputLimit}
	stderr := &limitedBuffer{limit: p.outputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := int(time.Since(start).Milliseconds())

	res := runnerapi.RunResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: duration,
		MemoryKB:   sandbox.MaxRSSkb(cmd),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case runErr == nil:
		res.Status = runnerapi.StatusSuccess
	case strings.Contains(res.Stderr, "MemoryError"):
		res.Status = runnerapi.StatusMemoryExceeded
	default:
		res.Status = runnerapi.StatusRuntimeError
	}
	return res
}
