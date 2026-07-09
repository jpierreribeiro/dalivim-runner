package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

type handler struct {
	svc            *executor.Service
	maxSourceBytes int
}

// run handles POST /run: the generic, language-dispatched entry point. The
// request names its own language; dispatch and limit enforcement live in the
// executor.
func (h *handler) run(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	if req.Language == "" {
		http.Error(w, "language is required", http.StatusBadRequest)
		return
	}
	h.execute(r.Context(), w, req)
}

// runPythonCompat handles the DEPRECATED POST /run/python. It injects
// language=python so the pre-extraction gateway keeps working without a code
// change; new callers should use POST /run with an explicit language.
func (h *handler) runPythonCompat(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	req.Language = "python"
	h.execute(r.Context(), w, req)
}

// decode reads and validates the request body, bounding it so an oversized
// payload cannot exhaust memory before the length check.
func (h *handler) decode(w http.ResponseWriter, r *http.Request) (runnerapi.RunRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(h.maxSourceBytes)+64*1024)
	var req runnerapi.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	if len(req.SourceCode) == 0 || len(req.SourceCode) > h.maxSourceBytes {
		http.Error(w, "source_code missing or too large", http.StatusBadRequest)
		return runnerapi.RunRequest{}, false
	}
	return req, true
}

func (h *handler) execute(ctx context.Context, w http.ResponseWriter, req runnerapi.RunRequest) {
	res, err := h.svc.Run(ctx, req)
	if errors.Is(err, executor.ErrUnsupportedLanguage) {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// A per-run infra failure OF OUR OWN (the sandbox/jail could not be stood up
	// for this run) surfaces as internal_error from the runtime. Emit it as 503 +
	// Retry-After rather than a terminal 200 so the Gateway treats it as a provider
	// failure and can fall back to another backend / retry, per
	// RUNNER_AUDIT_AND_CONTRACT.md §2.4.1. Student-code outcomes (success,
	// runtime_error, timeout, memory_exceeded, compile_error) stay 200 — they are
	// deterministic and a fallback would only repeat them. The body still carries
	// the RunResult so a Gateway that logs it keeps the detail.
	if res.Status == runnerapi.StatusInternalError {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(res)
}

// health is the unauthenticated liveness probe.
func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
