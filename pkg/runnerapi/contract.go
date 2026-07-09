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

	// CompileTimeoutMs bounds the compile phase of a compiled language (C/C++),
	// separate from TimeoutMs which bounds execution. Ignored for interpreted
	// languages. Falls back to the service default when zero, clamped to a ceiling.
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

	// CompileOutput carries the compiler's diagnostics (gcc/g++ stderr) for a
	// compiled language; it is the body of a compile_error and is otherwise empty.
	// Interpreted languages never set it.
	CompileOutput string `json:"compile_output,omitempty"`

	// Signal is the name of the signal that killed the process ("SIGSEGV",
	// "SIGKILL", …) when it died by one, else empty — it disambiguates a crash
	// (SIGSEGV) from a clean non-zero exit, and an OOM/seccomp kill (SIGKILL/SIGSYS)
	// from a normal one. Most relevant for compiled languages.
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
	StatusCompileError   = "compile_error" // compiled languages: the compile phase failed
	StatusInternalError  = "internal_error"
)
