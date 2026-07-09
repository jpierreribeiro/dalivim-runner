// Command runner is the dalivim code-execution microservice. It executes
// untrusted code in an isolated process behind a pre-shared-key gate: a hostile
// boundary that applies timeouts, output caps, best-effort memory limits, network
// isolation, and a fork-bomb cap, and must never share a process with the calling
// API.
//
// This file is the composition root only: read config, resolve the sandbox, wire
// the executor and HTTP server, and run until a termination signal.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jpierreribeiro/dalivim-runner/internal/config"
	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/internal/transport/httpapi"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(1)
	}
	if cfg.ServiceToken == "" {
		slog.Warn("RUNNER_SERVICE_TOKEN is empty (development); the runner accepts unauthenticated calls")
	}

	// Resolve network isolation and the fork-bomb cap before accepting a single
	// request. Configure fails closed when RUNNER_NETWORK_ISOLATION=require and
	// the platform forbids unprivileged namespaces.
	sb, err := sandbox.Configure(cfg.NetworkPolicy)
	if err != nil {
		slog.Error("network isolation", "err", err)
		os.Exit(1)
	}
	if err := sandbox.LimitProcesses(cfg.MaxProcesses); err != nil {
		slog.Warn("could not set process limit", "err", err)
	}

	svc := executor.NewService(
		executor.Limits{
			DefaultTimeout: cfg.DefaultTimeoutMs,
			MaxTimeoutMs:   cfg.MaxTimeoutMs,
			DefaultMemory:  cfg.DefaultMemoryMB,
			MaxMemoryMB:    cfg.MaxMemoryMB,
		},
		executor.NewPython(sb, cfg.MaxOutputBytes),
	)

	srv := httpapi.New(svc, httpapi.Config{
		Addr:           cfg.Addr,
		Token:          cfg.ServiceToken,
		MaxSourceBytes: cfg.MaxSourceBytes,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
