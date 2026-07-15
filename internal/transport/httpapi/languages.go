package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
)

// languagesResponse is the GET /languages payload (G12): the runtime catalog plus
// the effective limit policy. It carries only capability, never secrets, so the
// route is unauthenticated — same posture as /readyz.
type languagesResponse struct {
	Languages []executor.LanguageInfo `json:"languages"`
	Limits    limitsInfo              `json:"limits"`
}

// limitsInfo is the effective limit policy a caller should validate a request
// against before sending it. It merges the executor's run-limit ceilings (from the
// service) with the transport-level body/batch caps (held on the handler).
type limitsInfo struct {
	DefaultTimeoutMs        int `json:"default_timeout_ms"`
	MaxTimeoutMs            int `json:"max_timeout_ms"`
	DefaultMemoryMB         int `json:"default_memory_mb"`
	MaxMemoryMB             int `json:"max_memory_mb"`
	MaxOutputBytes          int `json:"max_output_bytes"`
	DefaultCompileTimeoutMs int `json:"default_compile_timeout_ms"`
	MaxCompileTimeoutMs     int `json:"max_compile_timeout_ms"`
	MaxSourceBytes          int `json:"max_source_bytes"`
	MaxStdinBytes           int `json:"max_stdin_bytes"`
	MaxFiles                int `json:"max_files"`
	MaxFileBytes            int `json:"max_file_bytes"`
	MaxFilesBytes           int `json:"max_files_bytes"`
	MaxBatch                int `json:"max_batch"`
	MaxBatchStdinBytes      int `json:"max_batch_stdin_bytes"`
}

// languages is the unauthenticated capability-discovery endpoint (G12). It reports
// the registered languages (id, version, kind, multifile/batch flags) and the
// effective limits, so a consumer can render a language picker and validate a
// request without a hardcoded, drift-prone list. It reveals only capability — no
// secrets — like /readyz, and is static: built from boot-resolved immutable state,
// so there is no per-request work beyond serialization.
func (h *handler) languages(w http.ResponseWriter, _ *http.Request) {
	lim := h.svc.Limits()
	resp := languagesResponse{
		Languages: h.svc.Catalog(),
		Limits: limitsInfo{
			DefaultTimeoutMs:        lim.DefaultTimeout,
			MaxTimeoutMs:            lim.MaxTimeoutMs,
			DefaultMemoryMB:         lim.DefaultMemory,
			MaxMemoryMB:             lim.MaxMemoryMB,
			MaxOutputBytes:          lim.MaxOutputBytes,
			DefaultCompileTimeoutMs: lim.DefaultCompileTimeout,
			MaxCompileTimeoutMs:     lim.MaxCompileTimeoutMs,
			MaxSourceBytes:          h.maxSourceBytes,
			MaxStdinBytes:           h.maxStdinBytes,
			MaxFiles:                lim.Files.MaxFiles,
			MaxFileBytes:            lim.Files.MaxFileBytes,
			MaxFilesBytes:           h.maxFilesBytes,
			MaxBatch:                h.maxBatch,
			MaxBatchStdinBytes:      h.maxBatchStdinBytes,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
