// Package runnerapi is the public wire contract of the dalivim-runner service:
// the exact JSON the gateway (main API) sends to POST /run and receives back.
//
// It has zero dependencies on purpose. The calling service may import it so both
// sides share a single source of truth for the schema instead of hand-mirroring
// structs across repositories.
package runnerapi

// RunRequest is the body of POST /run.
//
// The runner is a language-AGNOSTIC executor, so the request names its own
// language rather than the endpoint encoding it. Only Language and SourceCode
// are required; each limit falls back to the service default when left zero, and
// is clamped to the service's hard ceiling.
type RunRequest struct {
	Language   string `json:"language"`
	SourceCode string `json:"source_code"`
	Stdin      string `json:"stdin,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	MemoryMB   int    `json:"memory_mb,omitempty"`

	// CompileTimeoutMs is the wall-clock budget for the COMPILE phase of a compiled
	// language (c, cpp); interpreted languages ignore it. 0 => the service default.
	// The run phase is still governed by TimeoutMs. Clamped to the service ceiling.
	CompileTimeoutMs int `json:"compile_timeout_ms,omitempty"`
}

// RunResult is the response of POST /run.
type RunResult struct {
	Status         string `json:"status"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exit_code"`
	DurationMs     int    `json:"duration_ms"`
	MemoryKB       int    `json:"memory_kb"`
	RuntimeName    string `json:"runtime_name"`
	RuntimeVersion string `json:"runtime_version"`

	// CompileOutput is the compiler's stderr for a compiled language (the diagnostic
	// text on StatusCompileError, or warnings on success); empty for interpreted
	// languages. Kept separate from Stderr so a compile diagnostic is never confused
	// with the program's own runtime output.
	CompileOutput string `json:"compile_output,omitempty"`

	// Signal is the name of the signal that killed the process (e.g. "SIGSEGV"),
	// or empty when it exited normally. Best-effort: it is reliable on the netns
	// backend (the child's own wait status) but the nsjail backend reports nsjail's
	// status, so precise signal attribution under nsjail is F-E/F-F work (R6).
	Signal string `json:"signal,omitempty"`

	// PythonVersion is a DEPRECATED alias of RuntimeVersion, populated with the
	// same value so the current gateway adapter (which reads python_version) keeps
	// working unchanged during the extraction. Drop it once every caller reads
	// runtime_version.
	PythonVersion string `json:"python_version,omitempty"`
}

// Status is the closed set of execution outcomes. A run that reaches a runtime
// always ends in one of these; internal_error means the failure is ours, not the
// submitted code's.
const (
	StatusSuccess        = "success"
	StatusRuntimeError   = "runtime_error"
	StatusTimeout        = "timeout"
	StatusMemoryExceeded = "memory_exceeded"
	StatusCompileError   = "compile_error" // compiled language failed to build; see CompileOutput
	StatusInternalError  = "internal_error"
)
