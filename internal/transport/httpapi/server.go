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
	http *http.Server
}

// Config carries the transport-level knobs.
type Config struct {
	Addr              string
	Token             string
	MaxSourceBytes    int
	MaxStdinBytes     int
	MaxConcurrentRuns int

	// MetricsToken gates GET /metrics; empty falls back to Token so /metrics is
	// never public in production (metrics leak submission volume/patterns).
	MetricsToken string

	// Readiness posture (G4.3), resolved from the sandbox at boot.
	Backend             string // "nsjail" / "netns" / "none"
	NetworkIsolated     bool
	ReadyRequiresNsjail bool // /readyz returns 503 unless nsjail is the active backend
}

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
		backend:             cfg.Backend,
		networkIsolated:     cfg.NetworkIsolated,
		readyRequiresNsjail: cfg.ReadyRequiresNsjail,
	}

	// gate wraps an execution handler: authenticate first (RequireToken), then
	// bound concurrency. Ordering matters — only authenticated callers may consume
	// a run slot, so an unauthenticated flood cannot exhaust capacity.
	gate := func(next http.Handler) http.Handler {
		return RequireToken(cfg.Token, LimitConcurrency(cfg.MaxConcurrentRuns, m, next))
	}

	// /metrics is gated by its own token, or the service token when unset — never
	// public. Falls through to the pass-through gate only in dev (both empty).
	metricsToken := cfg.MetricsToken
	if metricsToken == "" {
		metricsToken = cfg.Token
	}

	mux := http.NewServeMux()
	mux.Handle("POST /run", gate(http.HandlerFunc(h.run)))
	mux.Handle("POST /run/python", gate(http.HandlerFunc(h.runPythonCompat)))
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.Handle("GET /metrics", RequireToken(metricsToken, http.HandlerFunc(h.serveMetrics)))

	return &Server{
		http: &http.Server{
			Addr:    cfg.Addr,
			Handler: mux,
			// Guards against slowloris-style header stalls. The run handler enforces
			// its own per-request execution timeout, so no overall Read/Write timeout
			// is set — a legitimate run may take several seconds.
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
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

	slog.Info("runner shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("runner shutdown complete")
	return nil
}
