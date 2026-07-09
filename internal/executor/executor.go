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

	// Compile-phase budget for compiled languages (C/C++). Clamped here, in the
	// one place limits are enforced, so a request may lower its compile bound but
	// never raise it past the ceiling (a long compile is a DoS vector). Interpreted
	// languages ignore the resulting value.
	DefaultCompileTimeout int // ms, applied when the request omits compile_timeout_ms
	MaxCompileTimeoutMs   int // ms, hard ceiling
}

// Floors are optional per-language lower bounds on a request's run limits,
// applied AFTER the default/ceiling clamp (G6). They exist for runtimes whose
// fixed baseline cost makes a small budget wrong for every program — the JVM's
// non-heap overhead sits on top of the -Xmx heap, so a python-sized memory_mb
// cannot even start the VM. A floor is a correctness aid, not a policy knob:
// the operator's global ceilings are absolute and a floor never lifts a limit
// past MaxTimeoutMs/MaxMemoryMB.
type Floors struct {
	TimeoutMs int // 0 = no floor
	MemoryMB  int // 0 = no floor
}

// limitFloorer is implemented by a Runtime whose language declares Floors. The
// service discovers it by assertion so the Runtime interface stays minimal and
// languages without floors are untouched.
type limitFloorer interface {
	LimitFloors() Floors
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
	req.CompileTimeoutMs = clamp(req.CompileTimeoutMs, s.limits.DefaultCompileTimeout, s.limits.MaxCompileTimeoutMs)
	if lf, ok := rt.(limitFloorer); ok {
		// Per-language floors (G6) raise an undersized budget so the runtime can
		// start at all; the global ceiling still wins over any floor.
		f := lf.LimitFloors()
		req.TimeoutMs = raiseToFloor(req.TimeoutMs, f.TimeoutMs, s.limits.MaxTimeoutMs)
		req.MemoryMB = raiseToFloor(req.MemoryMB, f.MemoryMB, s.limits.MaxMemoryMB)
	}

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

// raiseToFloor lifts v to floor (when floor > 0) but never past max (when
// max > 0): a language floor is subordinate to the operator's global ceiling.
func raiseToFloor(v, floor, max int) int {
	if floor > 0 && v < floor {
		v = floor
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}
