// Package httpapi is the HTTP transport around the executor: routing, the
// pre-shared-key gate, request-size limits, and graceful shutdown. It contains no
// execution logic — that belongs to the executor package.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
	"github.com/jpierreribeiro/dalivim-runner/internal/metrics"
)

// Server wraps the standard library HTTP server with the runner's routes and
// lifecycle.
type Server struct {
	http          *http.Server
	shutdownGrace time.Duration
}

// Config carries the transport-level knobs.
type Config struct {
	Addr string
	// Token is a single valid service token; Tokens is the full valid set (G8.2,
	// zero-downtime rotation). They are unioned, so callers may set either or both
	// — a request authorizes if it matches any. Empty (both) is dev-only.
	Token             string
	Tokens            []string
	MaxSourceBytes    int
	MaxStdinBytes     int
	MaxFilesBytes     int // multi-file total-content budget, for the body-size ceiling
	MaxConcurrentRuns int

	// Batch (stdins[]) request caps (G6): most inputs one request may carry, and
	// the summed stdin bytes across them. Per-element stdin still obeys
	// MaxStdinBytes. The batch-wide wall budget is the executor's, not ours.
	MaxBatch           int
	MaxBatchStdinBytes int

	// MetricsToken / MetricsTokens gate GET /metrics; when both are empty they fall
	// back to the service token set so /metrics is never public in production
	// (metrics leak submission volume/patterns).
	MetricsToken  string
	MetricsTokens []string

	// ShutdownGrace bounds how long ListenAndServe drains in-flight runs on
	// SIGTERM (G8.3). It must be ≥ the worst-case single-request wall-time so a
	// deploy never kills a run inside its own deadline; config derives it from the
	// run/compile ceilings. Zero falls back to defaultShutdownGrace.
	ShutdownGrace time.Duration

	// Readiness posture (G4.3), resolved from the sandbox at boot.
	Backend         string // "nsjail" / "netns" / "none"
	NetworkIsolated bool
	// MemoryAccounting is the per-run memory-bound posture ("cgroup-v2:<parent>" or
	// "rlimit-only"), reported on /readyz so a caller (the escape corpus) can tell
	// whether the RLIMIT_AS-incompatible runtimes are contained on a memory bomb here.
	MemoryAccounting    string
	ReadyRequiresNsjail bool // /readyz returns 503 unless nsjail is the active backend
}

// defaultShutdownGrace is the drain window when Config.ShutdownGrace is unset
// (e.g. tests). Production derives a larger, invariant-respecting value in config.
const defaultShutdownGrace = 15 * time.Second

// New builds the server: POST /run (and the deprecated POST /run/python alias)
// behind the token gate; unauthenticated GET /healthz (liveness) and GET /readyz
// (containment-aware readiness, G4.3); and token-gated GET /metrics (Prometheus,
// G4.1). Method+path routing (Go 1.22+) makes a wrong method a 405 without any
// per-handler checks.
func New(svc *executor.Service, cfg Config) *Server {
	m := metrics.New()
	h := &handler{
		svc:                 svc,
		metrics:             m,
		maxSourceBytes:      cfg.MaxSourceBytes,
		maxStdinBytes:       cfg.MaxStdinBytes,
		maxFilesBytes:       cfg.MaxFilesBytes,
		maxBatch:            cfg.MaxBatch,
		maxBatchStdinBytes:  cfg.MaxBatchStdinBytes,
		backend:             cfg.Backend,
		networkIsolated:     cfg.NetworkIsolated,
		memoryAccounting:    cfg.MemoryAccounting,
		readyRequiresNsjail: cfg.ReadyRequiresNsjail,
	}

	// The valid service-token set is the union of the singular Token and the plural
	// Tokens (G8.2), so a caller may configure either or both and rotation just adds
	// a second entry.
	serviceTokens := unionTokens(cfg.Token, cfg.Tokens)

	// gate wraps an execution handler: authenticate first (RequireTokens), then
	// bound concurrency. Ordering matters — only authenticated callers may consume
	// a run slot, so an unauthenticated flood cannot exhaust capacity.
	gate := func(next http.Handler) http.Handler {
		return RequireTokens(serviceTokens, LimitConcurrency(cfg.MaxConcurrentRuns, m, next))
	}

	// /metrics is gated by its own token set, or the service set when unset — never
	// public. Falls through to the pass-through gate only in dev (all empty).
	metricsTokens := unionTokens(cfg.MetricsToken, cfg.MetricsTokens)
	if len(metricsTokens) == 0 {
		metricsTokens = serviceTokens
	}

	mux := http.NewServeMux()
	mux.Handle("POST /run", gate(http.HandlerFunc(h.run)))
	mux.Handle("POST /run/python", gate(http.HandlerFunc(h.runPythonCompat)))
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.Handle("GET /metrics", RequireTokens(metricsTokens, http.HandlerFunc(h.serveMetrics)))

	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	return &Server{
		http: &http.Server{
			Addr:    cfg.Addr,
			Handler: mux,
			// Guards against slowloris-style header stalls. The run handler enforces
			// its own per-request execution timeout, so no overall Read/Write timeout
			// is set — a legitimate run may take several seconds.
			ReadHeaderTimeout: 10 * time.Second,
		},
		shutdownGrace: grace,
	}
}

// unionTokens merges a singular token and a token slice into one deduplicated
// set, dropping blanks. It lets a caller populate Config with either field (or
// both) and get a single authoritative valid set for RequireTokens.
func unionTokens(single string, many []string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(t string) {
		if t == "" {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	add(single)
	for _, t := range many {
		add(t)
	}
	return out
}

// Handler exposes the router for tests.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ListenAndServe serves until ctx is cancelled (SIGINT/SIGTERM), then drains
// in-flight requests within a bounded grace period.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("runner listening", "addr", s.http.Addr)
		errCh <- s.http.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Drain within a grace period sized to outlast the worst-case in-flight run
	// (G8.3), so a deploy SIGTERM lets a long run finish inside its own deadline
	// instead of killing it and surfacing a spurious failure to the student.
	slog.Info("runner shutdown started", "grace", s.shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownGrace)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("runner shutdown complete")
	return nil
}
