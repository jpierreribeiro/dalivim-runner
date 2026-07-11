// Package runnerapi is the public wire contract of the dalivim-runner service:
// the exact JSON the gateway (main API) sends to POST /run and receives back.
//
// It has zero dependencies on purpose. The calling service may import it so both
// sides share a single source of truth for the schema instead of hand-mirroring
// structs across repositories.
package runnerapi

// RunFile is one file of a multi-file submission: a caller-supplied relative
// path plus its content. The Path is the single most dangerous field in the
// whole contract — it is attacker-controlled and drives a filesystem write — so
// it is validated against a conservative grammar and materialized with a
// traversal-resistant API before anything touches disk (see the executor's
// file-policy and materialization layer). Path is relative to the per-run source
// root and uses forward slashes, e.g. "util.h", "src/main.c",
// "src/com/acme/Main.java".
type RunFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// RunRequest is the body of POST /run.
//
// The runner is a language-AGNOSTIC executor, so the request names its own
// language rather than the endpoint encoding it. Exactly one of SourceCode or
// Files carries the program; each limit falls back to the service default when
// left zero, and is clamped to the service's hard ceiling.
type RunRequest struct {
	Language string `json:"language"`

	// SourceCode is the single-file form: the whole program as one string,
	// written to the language's default entry file (main.py, Main.java, …). It is
	// mutually exclusive with Files. This is the original, unchanged contract; a
	// SourceCode request runs byte-for-byte as it always has.
	SourceCode string `json:"source_code,omitempty"`

	// Files is the multi-file form (G3): several named files materialized into a
	// per-run source tree, so a submission can span headers, translation units, a
	// package split, or sibling modules. Mutually exclusive with SourceCode.
	Files []RunFile `json:"files,omitempty"`

	// Entrypoint names which file (or, for Java, which class) is the program's
	// main. Meaningful only with Files; defaults per language when empty:
	//   - Python/JS/C/C++/Go: a relative path inside Files (main.py, main.go, …).
	//   - Java: a fully-qualified class name, e.g. "Main" or "com.acme.Main".
	Entrypoint string `json:"entrypoint,omitempty"`

	Stdin     string `json:"stdin,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	MemoryMB  int    `json:"memory_mb,omitempty"`

	// Stdins is the batch form (G6): the program is prepared once (for compiled
	// languages, compiled once) and executed once per element, each execution in
	// its own fresh jail with per-run limits unchanged. Mutually exclusive with
	// Stdin. The response becomes a BatchResult whose Results are index-aligned
	// with this slice. These are raw inputs, never "test cases" — the runner
	// holds no expected output and renders no verdict.
	Stdins []string `json:"stdins,omitempty"`

	// CompileTimeoutMs bounds the compile phase of a compiled language (C/C++),
	// separate from TimeoutMs which bounds execution. Ignored for interpreted
	// languages. Falls back to the service default when zero, clamped to a ceiling.
	CompileTimeoutMs int `json:"compile_timeout_ms,omitempty"`

	// Encoding selects the wire encoding of the binary data streams. "" or "utf8"
	// (default) treats Stdin/Stdins and the response Stdout/Stderr as UTF-8 text —
	// the original behaviour, in which a program emitting invalid UTF-8 has those
	// bytes replaced at the JSON boundary. "base64" makes the I/O binary-safe:
	// Stdin (and each Stdins element) is base64-DECODED before it reaches the
	// program, and the response Stdout/Stderr are base64-ENCODED from the raw
	// process bytes, so arbitrary/binary output round-trips without loss.
	// SourceCode/Files stay text (a program is source text); CompileOutput
	// (compiler diagnostics) is always text.
	Encoding string `json:"encoding,omitempty"`
}

// RunResult is the response of POST /run.
type RunResult struct {
	Status string `json:"status"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// StdoutTruncated / StderrTruncated report that the stream hit the output cap
	// and was cut: the captured Stdout/Stderr hold only the first cap bytes. This
	// is the AUTHORITATIVE truncation signal — a caller (e.g. the grader deciding
	// output_limit) must read this flag, never infer truncation from output length
	// or from the legacy "\n[output truncated]" marker still appended to the text.
	StdoutTruncated bool `json:"stdout_truncated"`
	StderrTruncated bool `json:"stderr_truncated"`

	ExitCode       int    `json:"exit_code"`
	DurationMs     int    `json:"duration_ms"`
	CompileMs      int    `json:"compile_ms,omitempty"` // compile-phase wall time (compiled languages); 0/omitted otherwise
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

// BatchResult is the response of POST /run when the request carries stdins[]
// (G6). The compile phase happens once, so its telemetry lives here on the
// envelope; Results carries one raw RunResult per input, index-aligned with the
// request's stdins. Provenance (runtime name/version) is stamped once on the
// envelope rather than repeated per result.
type BatchResult struct {
	// Status is the batch-level outcome: BatchStatusOK when the program was
	// prepared (compiled) and the inputs were executed; StatusCompileError when
	// the compile phase failed (Results is then empty — nothing was executed);
	// StatusInternalError when the failure is ours, not the submitted code's.
	Status         string `json:"status"`
	RuntimeName    string `json:"runtime_name"`
	RuntimeVersion string `json:"runtime_version"`

	// CompileMs / CompileOutput are the SHARED compile telemetry — the whole
	// point of the batch is that compilation happened once for every input.
	// Zero/empty for interpreted languages.
	CompileMs     int    `json:"compile_ms,omitempty"`
	CompileOutput string `json:"compile_output,omitempty"`

	// Results holds one RunResult per stdins element, in the same order
	// (results[i] ran against stdins[i]). Per-input failures are independent —
	// a timeout on one input never affects the others. Empty on compile_error.
	Results []RunResult `json:"results"`

	// Aborted means the batch-total wall budget was exhausted before every
	// input ran: Results is a partial prefix and the caller re-submits the rest.
	// It bounds how long one request can hold a concurrency slot; it is a
	// batch-level flag, not a per-run status.
	Aborted bool `json:"aborted,omitempty"`
}

// BatchStatusOK is the BatchResult status when the program was prepared and the
// batch executed (individually failing inputs included — their outcomes live in
// Results). It is distinct from the per-run status set below, which describes
// single executions.
const BatchStatusOK = "ok"

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

	// StatusOutputLimitExceeded means the run wrote more than the output cap and
	// was killed for it, rather than truncated-and-left-running. It is a fail-fast
	// verdict for an unbounded print loop: the captured Stdout/Stderr hold the
	// first cap bytes (with the truncation marker) and DurationMs is well under the
	// timeout. Additive — a caller that does not special-case it sees an
	// unsuccessful run with partial output.
	StatusOutputLimitExceeded = "output_limit_exceeded"
)
