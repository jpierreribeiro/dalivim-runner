package executor

import (
	"context"
	"errors"
	"fmt"
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
// additive.
type PythonRuntime struct {
	sandbox     *sandbox.Sandbox
	version     string
	outputLimit int
}

// NewPython builds the Python runtime. It detects the interpreter version once at
// construction so every result carries real provenance rather than a
// hand-configured value. outputLimit caps captured stdout/stderr.
func NewPython(sb *sandbox.Sandbox, outputLimit int) *PythonRuntime {
	return &PythonRuntime{
		sandbox:     sb,
		version:     detectPythonVersion(),
		outputLimit: outputLimit,
	}
}

func (p *PythonRuntime) Language() string { return "python" }
func (p *PythonRuntime) Version() string  { return p.version }

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

	// Per-run caps set inside the child shell before exec'ing python. Only -v
	// (address space) and -t (CPU seconds) are used: the container's /bin/sh is
	// dash, which lacks `ulimit -u`, so the process cap (RLIMIT_NPROC, fork-bomb
	// containment) is applied process-wide at startup instead. -t contains
	// CPU-bound loops even when wall-clock cancellation races.
	cpuSeconds := (req.TimeoutMs+999)/1000 + 1 // CPU cap just above the wall-clock timeout
	shellCmd := fmt.Sprintf("ulimit -v %d; ulimit -t %d; exec python3 -I main.py", req.MemoryMB*1024, cpuSeconds)
	//#nosec G204 -- executing submitted code is the runner's purpose; the shell string interpolates only integer limits, the source is written to main.py (not the command), and the process runs sandboxed (empty netns, rlimits, restricted PATH/env).
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(req.Stdin)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONUNBUFFERED=1"}
	cmd.SysProcAttr = p.sandbox.SysProcAttr()
	cmd.Cancel = sandbox.CancelCmd(cmd)

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
