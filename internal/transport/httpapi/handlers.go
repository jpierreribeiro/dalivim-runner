package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
	"github.com/jpierreribeiro/dalivim-runner/internal/metrics"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

type handler struct {
	svc                *executor.Service
	metrics            *metrics.Metrics
	maxSourceBytes     int
	maxStdinBytes      int
	maxFilesBytes      int
	maxBatch           int // most stdins[] elements a batch request may carry (G6)
	maxBatchStdinBytes int // summed stdin bytes across a batch (G6)

	// readiness posture (G4.3), resolved once at boot.
	backend             string // active sandbox backend ("nsjail"/"netns"/"none")
	networkIsolated     bool
	memoryAccounting    string // "cgroup-v2:<parent>" or "rlimit-only"
	readyRequiresNsjail bool
}

// run handles POST /run: the generic, language-dispatched entry point. The
// request names its own language; dispatch and limit enforcement live in the
// executor.
func (h *handler) run(w http.ResponseWriter, r *http.Request) {
	reqID := h.ensureRequestID(w, r)
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	if req.Language == "" {
		http.Error(w, "language is required", http.StatusBadRequest)
		return
	}
	h.execute(r.Context(), w, req, reqID)
}

// runPythonCompat handles the DEPRECATED POST /run/python. It injects
// language=python so the pre-extraction gateway keeps working without a code
// change; new callers should use POST /run with an explicit language.
func (h *handler) runPythonCompat(w http.ResponseWriter, r *http.Request) {
	reqID := h.ensureRequestID(w, r)
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	req.Language = "python"
	h.execute(r.Context(), w, req, reqID)
}

// ensureRequestID echoes the backend's X-Request-ID (or mints one) so a single
// submission can be traced across the backend and runner logs. Set before any
// write so it rides even on a 4xx.
func (h *handler) ensureRequestID(w http.ResponseWriter, r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if id == "" || len(id) > 200 {
		id = newRequestID()
	}
	w.Header().Set("X-Request-ID", id)
	return id
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// decode reads and validates the request body, bounding it so an oversized
// payload cannot exhaust memory before the length check. The body ceiling covers
// the larger of the single-file source budget and the multi-file total budget,
// plus stdin and JSON slack, so neither legitimate large stdin (judge inputs
// commonly exceed 64 KiB) nor a full multi-file payload is cut off at the
// transport before its own check.
//
// It performs only coarse, language-agnostic bounds here (raw source size, stdin
// size). The submission-shape rules — exactly one of source_code/files, the path
// grammar, per-file/total caps, extensions, and entrypoint — live in the executor
// (Service.Run) so they are enforced in one place against the per-language policy;
// a *executor.ValidationError from there is mapped to 400 by execute.
func (h *handler) decode(w http.ResponseWriter, r *http.Request) (runnerapi.RunRequest, bool) {
	bodyCap := int64(h.maxSourceBytes)
	if int64(h.maxFilesBytes) > bodyCap {
		bodyCap = int64(h.maxFilesBytes)
	}
	stdinCap := int64(h.maxStdinBytes)
	if int64(h.maxBatchStdinBytes) > stdinCap {
		stdinCap = int64(h.maxBatchStdinBytes) // a batch may carry more total stdin than one run
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyCap+stdinCap+64*1024)
	var req runnerapi.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	// Binary-safe I/O (opt-in, Judge0-style): base64-DECODE the input streams up
	// front so the size checks below and the executor both see raw bytes; the
	// executor base64-ENCODES the output symmetrically. Default (utf8/"") is the
	// unchanged text passthrough. Applies to the data streams only, never source.
	switch strings.ToLower(strings.TrimSpace(req.Encoding)) {
	case "", "utf8":
		// text passthrough (default)
	case "base64":
		dec, derr := base64.StdEncoding.DecodeString(req.Stdin)
		if derr != nil {
			http.Error(w, "invalid base64 in stdin", http.StatusBadRequest)
			return runnerapi.RunRequest{}, false
		}
		req.Stdin = string(dec)
		for i, in := range req.Stdins {
			d, ierr := base64.StdEncoding.DecodeString(in)
			if ierr != nil {
				http.Error(w, "invalid base64 in stdins", http.StatusBadRequest)
				return runnerapi.RunRequest{}, false
			}
			req.Stdins[i] = string(d)
		}
	default:
		http.Error(w, `unsupported encoding (use "utf8" or "base64")`, http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	if len(req.SourceCode) > h.maxSourceBytes {
		http.Error(w, "source_code too large", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	if h.maxStdinBytes > 0 && len(req.Stdin) > h.maxStdinBytes {
		http.Error(w, "stdin too large", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	// Batch (stdins[]) size caps (G6): the input count, each element (same cap as
	// a single stdin), and the summed bytes — all before anything executes. The
	// deeper shape rules (stdin/stdins exclusivity) live in the executor.
	if n := len(req.Stdins); n > 0 {
		if h.maxBatch > 0 && n > h.maxBatch {
			http.Error(w, "too many stdins", http.StatusBadRequest)
			return runnerapi.RunRequest{}, false
		}
		total := 0
		for _, in := range req.Stdins {
			if h.maxStdinBytes > 0 && len(in) > h.maxStdinBytes {
				http.Error(w, "stdin element too large", http.StatusBadRequest)
				return runnerapi.RunRequest{}, false
			}
			total += len(in)
		}
		if h.maxBatchStdinBytes > 0 && total > h.maxBatchStdinBytes {
			http.Error(w, "batch stdin total too large", http.StatusBadRequest)
			return runnerapi.RunRequest{}, false
		}
	}
	return req, true
}

func (h *handler) execute(ctx context.Context, w http.ResponseWriter, req runnerapi.RunRequest, reqID string) {
	if len(req.Stdins) > 0 {
		h.executeBatch(ctx, w, req, reqID)
		return
	}
	res, err := h.svc.Run(ctx, req)
	if errors.Is(err, executor.ErrUnsupportedLanguage) {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	// A rejected submission (bad path, too many files, forbidden extension, missing
	// entrypoint, both/neither of source_code/files, …) is a client error: 400 with
	// the specific reason, never a run outcome.
	var ve *executor.ValidationError
	if errors.As(err, &ve) {
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Telemetry: one payload-free structured line + the metric series. NEVER log or
	// label source_code/stdin/stdout — those are student data; only outcomes and
	// sizes/timings (G4.1/G4.2). RuntimeName is the resolved language.
	lang := res.RuntimeName
	if lang == "" {
		lang = req.Language
	}
	h.metrics.ObserveRun(lang, res.Status, res.DurationMs, res.CompileMs)
	slog.Info("run",
		"request_id", reqID,
		"language", lang,
		"status", res.Status,
		"duration_ms", res.DurationMs,
		"compile_ms", res.CompileMs,
		"memory_kb", res.MemoryKB,
		"exit_code", res.ExitCode,
		"signal", res.Signal,
		"truncated", strings.Contains(res.Stdout, outputTruncatedMarker) || strings.Contains(res.Stderr, outputTruncatedMarker),
	)

	w.Header().Set("Content-Type", "application/json")
	// A per-run infra failure OF OUR OWN (the sandbox/jail could not be stood up
	// for this run) surfaces as internal_error from the runtime. Emit it as 503 +
	// Retry-After rather than a terminal 200 so the Gateway treats it as a provider
	// failure and can fall back to another backend / retry, per
	// RUNNER_AUDIT_AND_CONTRACT.md §2.4.1. Student-code outcomes (success,
	// runtime_error, timeout, memory_exceeded, compile_error, output_limit_exceeded)
	// stay 200 — deterministic; a fallback would only repeat them. The body still
	// carries the RunResult so a Gateway that logs it keeps the detail.
	if res.Status == runnerapi.StatusInternalError {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(res)
}

// executeBatch runs a stdins[] request (G6) and encodes the batch envelope. It
// mirrors execute: 400 for validation, 503 (+Retry-After) when the failure is
// ours, 200 for every student-code outcome — including a compile_error batch
// and an Aborted partial batch, both deterministic.
func (h *handler) executeBatch(ctx context.Context, w http.ResponseWriter, req runnerapi.RunRequest, reqID string) {
	res, err := h.svc.RunBatch(ctx, req)
	if errors.Is(err, executor.ErrUnsupportedLanguage) {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	var ve *executor.ValidationError
	if errors.As(err, &ve) {
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Telemetry: per-input series (so batch runs count in the same metrics as
	// single runs) with the shared compile time observed once, plus ONE
	// payload-free batch log line — never per input, and never any student data.
	lang := res.RuntimeName
	if lang == "" {
		lang = req.Language
	}
	if len(res.Results) == 0 {
		// compile_error / internal_error: nothing executed; record the compile.
		h.metrics.ObserveRun(lang, res.Status, 0, res.CompileMs)
	}
	totalMs := 0
	for i, r := range res.Results {
		compileMs := 0
		if i == 0 {
			compileMs = res.CompileMs // the shared compile, observed exactly once
		}
		h.metrics.ObserveRun(lang, r.Status, r.DurationMs, compileMs)
		totalMs += r.DurationMs
	}
	slog.Info("batch",
		"request_id", reqID,
		"language", lang,
		"status", res.Status,
		"inputs", len(req.Stdins),
		"results", len(res.Results),
		"aborted", res.Aborted,
		"compile_ms", res.CompileMs,
		"total_run_ms", totalMs,
	)

	w.Header().Set("Content-Type", "application/json")
	// Same failover contract as a single run: OUR infra failure is 503 +
	// Retry-After so the gateway retries/fails over; student-code outcomes stay
	// 200. Per-input internal_error inside Results is not escalated — the batch
	// itself completed and the caller sees exactly which inputs need a re-run.
	if res.Status == runnerapi.StatusInternalError {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(res)
}

// outputTruncatedMarker is the suffix limitedBuffer appends when a stream was
// truncated (kept in sync with the executor's marker).
const outputTruncatedMarker = "[output truncated]"

// health is the unauthenticated liveness probe: is the process up.
func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ready is the readiness probe (G4.3): 200 only when containment is actually
// engaged, else 503 — so an instance silently degraded to the weak netns fallback
// under RUNNER_SANDBOX=auto is taken out of rotation instead of quietly running
// untrusted code less contained. Reports the resolved posture (no secrets), so it
// may be public. Distinct from /healthz, which only answers "is the process up".
func (h *handler) ready(w http.ResponseWriter, _ *http.Request) {
	ok := !h.readyRequiresNsjail || h.backend == "nsjail"
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":            ok,
		"backend":          h.backend,
		"network_isolated": h.networkIsolated,
		// The per-run memory-bound posture: "cgroup-v2:<parent>" means each run has
		// an authoritative memory.max (so the RLIMIT_AS-incompatible runtimes —
		// Go/JS/Java — are contained on a memory bomb and classified memory_exceeded
		// from the kernel OOM event); "rlimit-only" means memory is bounded by
		// RLIMIT_AS / the interpreter heap flag alone. The escape corpus reads this to
		// decide whether to assert the Go/JS/Java memory-bomb cases here or defer them
		// to the on-target (cgroup-engaged) proof.
		"memory_accounting": h.memoryAccounting,
	})
}

// serveMetrics renders the Prometheus exposition. It is gated (see New) — metrics
// leak submission volume/patterns, so it is never public.
func (h *handler) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(h.metrics.Render()))
}
