// Package executor is the language-agnostic core: it clamps a request's limits
// to the service ceilings, dispatches to the runtime registered for the
// requested language, and stamps provenance on the result. It holds no HTTP
// knowledge and no OS-level syscall code — the transport layer and the sandbox
// package own those, respectively.
package executor

import (
	"context"
	"fmt"
	"sort"

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

	// Test-mode (G9) run budget, a SEPARATE and larger ceiling than the run-mode
	// timeout: a whole test suite legitimately runs longer than one program, so a
	// mode=test request is clamped to these instead of DefaultTimeout/MaxTimeoutMs.
	// Memory reuses the run-mode budget. Zero falls back to the run-mode values in
	// clampLimits, so an operator that leaves them unset gets today's behaviour.
	DefaultTestTimeout int // ms, applied when a mode=test request omits a timeout
	MaxTestTimeoutMs   int // ms, hard ceiling for a mode=test run

	// Compile-phase budget for compiled languages (C/C++). Clamped here, in the
	// one place limits are enforced, so a request may lower its compile bound but
	// never raise it past the ceiling (a long compile is a DoS vector). Interpreted
	// languages ignore the resulting value.
	DefaultCompileTimeout int // ms, applied when the request omits compile_timeout_ms
	MaxCompileTimeoutMs   int // ms, hard ceiling

	// Files are the multi-file submission caps (G3), enforced by the per-language
	// FilePolicy during request validation — the one place, like the timeouts,
	// that limits live.
	Files FileCaps

	// MaxBatchTotalMs is the batch-wide wall budget for a stdins[] request (G6):
	// when the cumulative wall time (compile included) crosses it, the batch
	// stops with Aborted=true and partial results. It is THE containment control
	// for batches — a batch holds one concurrency slot for its whole duration,
	// so worst-case pressure is MaxConcurrentRuns × MaxBatch × per-run timeout
	// without it. 0 disables the budget (not recommended outside tests).
	MaxBatchTotalMs int
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

// batchRunner is implemented by a Runtime that can amortize per-request setup
// across many stdins (G6): compiled languages compile once and loop the run
// jail; interpreted languages write the source once and loop the interpreter.
// Each input still executes in its own fresh jail — the amortized part is the
// preparation, never the containment. Discovered by assertion, like
// limitFloorer, so the Runtime interface stays minimal.
type batchRunner interface {
	// RunBatch executes req once per req.Stdins element, sequentially and
	// index-aligned, stopping early (Aborted) when totalBudgetMs of wall time —
	// compile included — is exhausted. Limits on req are already clamped.
	RunBatch(ctx context.Context, req runnerapi.RunRequest, totalBudgetMs int) runnerapi.BatchResult
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

// LanguageInfo describes one registered language for the GET /languages discovery
// endpoint (G12): its wire id, detected version, runtime kind, and capability
// flags. It is capability metadata only — never any secret or backend posture.
type LanguageInfo struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`      // "interpreted" | "compiled"
	MultiFile bool   `json:"multifile"` // accepts a files[] submission (G3)
	Batch     bool   `json:"batch"`     // accepts a stdins[] batch (G6)
	Test      bool   `json:"test"`      // accepts a mode=test test-runner request (G9)
}

// Catalog returns the registered languages with provenance and capability flags,
// sorted by id, for the discovery endpoint. It reads only the immutable registry
// resolved at construction (versions detected once, kinds/flags fixed), so it is
// safe for concurrent use and allocates a small, bounded response.
func (s *Service) Catalog() []LanguageInfo {
	out := make([]LanguageInfo, 0, len(s.runtimes))
	for id, rt := range s.runtimes {
		_, batch := rt.(batchRunner)
		out = append(out, LanguageInfo{
			ID:        id,
			Version:   rt.Version(),
			Kind:      runtimeKind(rt),
			MultiFile: supportsMultiFile(id),
			Batch:     batch,
			Test:      supportsTestMode(id),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Limits exposes the resolved limit policy for the discovery endpoint. It returns
// a value copy, so a caller cannot mutate the service's limits.
func (s *Service) Limits() Limits { return s.limits }

// runtimeKind names a runtime's shape for the catalog. It type-switches on the
// concrete runtime rather than widening the minimal Runtime interface with a
// Kind() method; a language whose type is neither reports "unknown".
func runtimeKind(rt Runtime) string {
	switch rt.(type) {
	case *compiledRuntime:
		return "compiled"
	case *interpretedRuntime:
		return "interpreted"
	default:
		return "unknown"
	}
}

// supportsMultiFile reports whether a language has a file policy — i.e. whether a
// files[] submission is accepted for it (the normalize path gates on the same map).
func supportsMultiFile(lang string) bool {
	_, ok := filePolicies[lang]
	return ok
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
	if len(req.Stdins) > 0 {
		// A stdins[] request must take the batch path (RunBatch) — rejecting it
		// here keeps the single-run contract crisp instead of silently ignoring
		// the extra inputs.
		return runnerapi.RunResult{}, invalid("stdins_on_single_run", "stdins requires the batch execution path")
	}
	if err := validateMode(req); err != nil {
		return runnerapi.RunResult{}, err
	}
	// Validate and normalize the submission shape (single-file sugar vs multi-file)
	// before anything runs. A *ValidationError here is a 400 the transport surfaces;
	// it is the fail-fast gate in front of materialization.
	normalized, err := s.normalize(req)
	if err != nil {
		return runnerapi.RunResult{}, err
	}
	req = s.clampLimits(normalized, rt)

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

// RunBatch is the stdins[] entry point (G6): one program, many inputs, many raw
// results. The submission shape and limits are validated/clamped exactly like a
// single run; the runtime then amortizes preparation (the compile, for compiled
// languages) across the inputs. The runner never compares outputs to anything —
// the results are raw and the backend judges.
func (s *Service) RunBatch(ctx context.Context, req runnerapi.RunRequest) (runnerapi.BatchResult, error) {
	rt, ok := s.runtimes[req.Language]
	if !ok {
		return runnerapi.BatchResult{}, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, req.Language)
	}
	if len(req.Stdins) == 0 {
		return runnerapi.BatchResult{}, invalid("empty_batch", "stdins must carry at least one input")
	}
	if req.Stdin != "" {
		return runnerapi.BatchResult{}, invalid("both_stdin_and_stdins", "exactly one of stdin or stdins may be set, not both")
	}
	if req.Mode == modeTest {
		// Test mode defines its own inputs and returns one report — a stdins[]
		// batch has no meaning for it. Reject rather than silently ignore (G9).
		return runnerapi.BatchResult{}, invalid("test_mode_batch", "mode=test does not support a stdins[] batch")
	}
	if err := validateMode(req); err != nil {
		return runnerapi.BatchResult{}, err
	}
	br, ok := rt.(batchRunner)
	if !ok {
		return runnerapi.BatchResult{}, invalid("unsupported_batch", "language %q does not support batch execution", req.Language)
	}
	normalized, err := s.normalize(req)
	if err != nil {
		return runnerapi.BatchResult{}, err
	}
	req = s.clampLimits(normalized, rt)

	res := br.RunBatch(ctx, req, s.limits.MaxBatchTotalMs)
	res.RuntimeName = rt.Language()
	if res.RuntimeVersion == "" {
		res.RuntimeVersion = rt.Version()
	}
	return res, nil
}

// clampLimits applies the limit policy to one request: defaults for omitted
// values, the global ceilings, then any per-language floors (G6) — the single
// place limits are enforced for both the single-run and batch paths.
func (s *Service) clampLimits(req runnerapi.RunRequest, rt Runtime) runnerapi.RunRequest {
	// Test mode (G9) uses a separate, larger timeout envelope — a suite runs longer
	// than one program. When the operator leaves the test ceilings unset they fall
	// back to the run-mode values, so behaviour is unchanged until they are set.
	defTimeout, maxTimeout := s.limits.DefaultTimeout, s.limits.MaxTimeoutMs
	if req.Mode == modeTest {
		if s.limits.DefaultTestTimeout > 0 {
			defTimeout = s.limits.DefaultTestTimeout
		}
		if s.limits.MaxTestTimeoutMs > 0 {
			maxTimeout = s.limits.MaxTestTimeoutMs
		}
	}
	req.TimeoutMs = clamp(req.TimeoutMs, defTimeout, maxTimeout)
	req.MemoryMB = clamp(req.MemoryMB, s.limits.DefaultMemory, s.limits.MaxMemoryMB)
	req.CompileTimeoutMs = clamp(req.CompileTimeoutMs, s.limits.DefaultCompileTimeout, s.limits.MaxCompileTimeoutMs)
	if lf, ok := rt.(limitFloorer); ok {
		// Per-language floors raise an undersized budget so the runtime can
		// start at all; the global ceiling (mode-appropriate) still wins over a floor.
		f := lf.LimitFloors()
		req.TimeoutMs = raiseToFloor(req.TimeoutMs, f.TimeoutMs, maxTimeout)
		req.MemoryMB = raiseToFloor(req.MemoryMB, f.MemoryMB, s.limits.MaxMemoryMB)
	}
	return req
}

// normalize enforces the submission contract and canonicalizes the request:
// exactly one of source_code / files must be set, and when files is set it is
// validated against the language's FilePolicy (path grammar, caps, extensions,
// entrypoint) and replaced with the canonical, path-sorted list plus the resolved
// entrypoint. A single-file source_code request passes through untouched so its
// behaviour is byte-for-byte unchanged. Returns a *ValidationError (→ HTTP 400)
// on any bad input.
func (s *Service) normalize(req runnerapi.RunRequest) (runnerapi.RunRequest, error) {
	hasSource := req.SourceCode != ""
	hasFiles := len(req.Files) > 0
	switch {
	case hasSource && hasFiles:
		return req, invalid("both_source_and_files", "exactly one of source_code or files must be set, not both")
	case !hasSource && !hasFiles:
		return req, invalid("no_program", "either source_code or files must be set")
	case hasSource:
		return req, nil // single-file sugar: unchanged legacy path
	}

	// Multi-file: validate against the per-language policy for the request's MODE.
	// The language is known (the runtime resolved) and, for test mode, its test
	// support was already checked by validateMode, so policyForMode cannot miss.
	policy, ok := policyForMode(req.Language, req.Mode, s.limits.Files)
	if !ok {
		return req, invalid("unsupported_language", "language %q does not support multi-file submissions", req.Language)
	}
	if req.Mode == modeTest {
		// Test mode has no entrypoint — the framework discovers its own tests over
		// the whole tree, so validate the files but resolve no main.
		files, err := validateTestFiles(req, policy)
		if err != nil {
			return req, err
		}
		req.Files = files
		req.Entrypoint = ""
		return req, nil
	}
	files, entry, err := validateFiles(req, policy)
	if err != nil {
		return req, err
	}
	req.Files = files
	req.Entrypoint = entry
	return req, nil
}

// validateMode enforces the closed set of execution modes (G9): "" / "run" (the
// unchanged program execution) and "test" (the per-language test framework). A
// test-mode request is admitted only for a language that has a test command
// registered; anything else is a 400 before normalization or materialization.
func validateMode(req runnerapi.RunRequest) error {
	switch req.Mode {
	case "", modeRun:
		return nil
	case modeTest:
		if !supportsTestMode(req.Language) {
			return invalid("unsupported_test_mode", "language %q does not support test mode", req.Language)
		}
		return nil
	default:
		return invalid("unsupported_mode", "mode must be \"run\" or \"test\" (got %q)", req.Mode)
	}
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
