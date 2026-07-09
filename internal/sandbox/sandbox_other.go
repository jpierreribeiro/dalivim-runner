//go:build !linux

package sandbox

import (
	"log/slog"
	"os/exec"
	"syscall"
)

// Sandbox is the degraded non-Linux stub. The runner's isolation guarantees —
// empty network namespace and RLIMIT_NPROC fork-bomb cap — are Linux-only. On any
// other OS the package still compiles and runs so the service can be built and
// exercised during local development, but it provides NO containment. Never
// deploy the runner off Linux.
type Sandbox struct{}

// NetworkIsolated always reports false off Linux.
func (s *Sandbox) NetworkIsolated() bool { return false }

// Configure ignores the policy off Linux and warns loudly that no in-process
// containment is available.
func Configure(_ string) (*Sandbox, error) {
	slog.Warn("network isolation UNAVAILABLE: this OS is not Linux; the runner provides NO in-process containment — local development only")
	return &Sandbox{}, nil
}

// SysProcAttr returns empty attributes; no namespace or process-group containment
// is available off Linux.
func (s *Sandbox) SysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// CancelCmd falls back to killing just the direct child process.
func CancelCmd(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

// LimitProcesses is a no-op off Linux (RLIMIT_NPROC semantics differ or are
// absent).
func LimitProcesses(_ int) error { return nil }

// MaxRSSkb is unavailable off Linux.
func MaxRSSkb(_ *exec.Cmd) int { return 0 }
