package httpapi

import (
	"context"
	"crypto/rand"
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
	svc            *executor.Service
	metrics        *metrics.Metrics
	maxSourceBytes int
	maxStdinBytes  int

	// readiness posture (G4.3), resolved once at boot.
	backend             string // active sandbox backend ("nsjail"/"netns"/"none")
	networkIsolated     bool
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
// source + stdin + JSON slack so a legitimate large stdin (judge inputs commonly
// exceed 64 KiB) is not cut off at the transport layer before its own check.
func (h *handler) decode(w http.ResponseWriter, r *http.Request) (runnerapi.RunRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(h.maxSourceBytes)+int64(h.maxStdinBytes)+64*1024)
	var req runnerapi.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	if len(req.SourceCode) == 0 || len(req.SourceCode) > h.maxSourceBytes {
		http.Error(w, "source_code missing or too large", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	if h.maxStdinBytes > 0 && len(req.Stdin) > h.maxStdinBytes {
		http.Error(w, "stdin too large", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	return req, true
}

func (h *handler) execute(ctx context.Context, w http.ResponseWriter, req runnerapi.RunRequest, reqID string) {
	res, err := h.svc.Run(ctx, req)
	if errors.Is(err, executor.ErrUnsupportedLanguage) {
		http.Error(w, "unsupported language", http.StatusBadRequest)
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
	})
}

// serveMetrics renders the Prometheus exposition. It is gated (see New) — metrics
// leak submission volume/patterns, so it is never public.
func (h *handler) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(h.metrics.Render()))
}
