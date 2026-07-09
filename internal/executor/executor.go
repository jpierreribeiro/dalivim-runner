// Package executor is the language-agnostic core: it clamps a request's limits
// to the service ceilings, dispatches to the runtime registered for the
// requested language, and stamps provenance on the result. It holds no HTTP
// knowledge and no OS-level syscall code — the transport layer and the sandbox
// package own those, respectively.
package executor

import (
	"context"
	"fmt"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// ErrUnsupportedLanguage is returned by Service.Run when no runtime is registered
// for the requested language. The transport layer maps it to 400.
var ErrUnsupportedLanguage = fmt.Errorf("unsupported language")

// Limits are the global ceilings every request is clamped to. They come from
// configuration and are never taken from the request itself.
type Limits struct {
	DefaultTimeout int // ms, applied when the request omits a timeout
	MaxTimeoutMs   int // ms, hard ceiling
	DefaultMemory  int // MB, applied when the request omits a memory limit
	MaxMemoryMB    int // MB, hard ceiling
}

// Runtime executes source code for exactly one language inside the sandbox.
// Adding a language is additive: implement Runtime and register it — dispatch and
// the HTTP layer never change.
type Runtime interface {
	// Language is the wire identifier callers send (e.g. "python").
	Language() string
	// Version is the detected runtime version (e.g. "3.12.3"), for provenance.
	Version() string
	// Run executes one request whose limits are already clamped. Code-level
	// failures are encoded in Result.Status, never returned as an error.
	Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult
}

// Service dispatches a request to the right runtime. It is the single entry
// point the transport layer calls.
type Service struct {
	runtimes map[string]Runtime
	limits   Limits
}

// NewService registers the given runtimes (keyed by Language) behind the limit
// policy.
func NewService(limits Limits, runtimes ...Runtime) *Service {
	m := make(map[string]Runtime, len(runtimes))
	for _, rt := range runtimes {
		m[rt.Language()] = rt
	}
	return &Service{runtimes: m, limits: limits}
}

// Languages returns the registered language identifiers (for diagnostics).
func (s *Service) Languages() []string {
	out := make([]string, 0, len(s.runtimes))
	for lang := range s.runtimes {
		out = append(out, lang)
	}
	return out
}

// Run clamps the request's limits, dispatches to the runtime, and fills
// provenance (runtime name/version and the deprecated python_version alias).
// ErrUnsupportedLanguage is the only error it returns; every execution outcome is
// carried in the result's Status.
func (s *Service) Run(ctx context.Context, req runnerapi.RunRequest) (runnerapi.RunResult, error) {
	rt, ok := s.runtimes[req.Language]
	if !ok {
		return runnerapi.RunResult{}, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, req.Language)
	}
	req.TimeoutMs = clamp(req.TimeoutMs, s.limits.DefaultTimeout, s.limits.MaxTimeoutMs)
	req.MemoryMB = clamp(req.MemoryMB, s.limits.DefaultMemory, s.limits.MaxMemoryMB)

	res := rt.Run(ctx, req)
	res.RuntimeName = rt.Language()
	if res.RuntimeVersion == "" {
		res.RuntimeVersion = rt.Version()
	}
	if req.Language == "python" {
		// Deprecated alias so the pre-extraction gateway (reads python_version)
		// keeps working; see runnerapi.RunResult.
		res.PythonVersion = res.RuntimeVersion
	}
	return res, nil
}

// clamp returns def when v <= 0, caps at max (when max > 0), else v.
func clamp(v, def, max int) int {
	if v <= 0 {
		v = def
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}
