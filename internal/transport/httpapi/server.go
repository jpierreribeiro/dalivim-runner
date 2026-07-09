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
)

// Server wraps the standard library HTTP server with the runner's routes and
// lifecycle.
type Server struct {
	http *http.Server
}

// Config carries the transport-level knobs.
type Config struct {
	Addr           string
	Token          string
	MaxSourceBytes int
}

// New builds the server: POST /run (and the deprecated POST /run/python alias)
// behind the token gate, plus an unauthenticated GET /healthz for liveness
// probes. Method+path routing (Go 1.22+) makes a wrong method a 405 without any
// per-handler checks.
func New(svc *executor.Service, cfg Config) *Server {
	h := &handler{svc: svc, maxSourceBytes: cfg.MaxSourceBytes}

	mux := http.NewServeMux()
	mux.Handle("POST /run", RequireToken(cfg.Token, http.HandlerFunc(h.run)))
	mux.Handle("POST /run/python", RequireToken(cfg.Token, http.HandlerFunc(h.runPythonCompat)))
	mux.HandleFunc("GET /healthz", h.health)

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
