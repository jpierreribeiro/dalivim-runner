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

	// Resolve the containment backend before accepting a single request.
	// Configure probes nsjail (RUNNER_SANDBOX) and, when it is unavailable, falls
	// back to the netns backend (RUNNER_NETWORK_ISOLATION) — or fails closed when
	// either dial is set to "require".
	//
	// Fork-bomb containment is deliberately NOT applied process-wide here.
	// RLIMIT_NPROC is enforced per real-uid, so lowering it globally throttles
	// every process this uid already runs and can make the runner itself fail to
	// fork ("errno=11") on a busy host. Per-run process caps belong to the nsjail
	// backend (--rlimit_nproc against a jail-private uid); overload is bounded
	// instead by the transport's concurrency limit (RUNNER_MAX_CONCURRENT_RUNS).
	sb, err := sandbox.Configure(cfg.SandboxPolicy, cfg.NetworkPolicy)
	if err != nil {
		slog.Error("sandbox", "err", err)
		os.Exit(1)
	}

	// Every runtime shares one sandbox and plugs in as a registry entry plus a
	// constructor, inheriting the identical jail. Interpreted languages run in one
	// phase; compiled languages (C/C++) build in a writable compile jail, then
	// execute the artifact in a separate read-only run jail.
	compileLimits := executor.CompileLimits{
		MemoryMB:         cfg.CompileMemoryMB,
		DefaultTimeoutMs: cfg.DefaultCompileTimeoutMs,
		MaxTimeoutMs:     cfg.MaxCompileTimeoutMs,
		MaxArtifactBytes: cfg.MaxArtifactBytes,
	}
	svc := executor.NewService(
		executor.Limits{
			DefaultTimeout: cfg.DefaultTimeoutMs,
			MaxTimeoutMs:   cfg.MaxTimeoutMs,
			DefaultMemory:  cfg.DefaultMemoryMB,
			MaxMemoryMB:    cfg.MaxMemoryMB,
		},
		executor.NewPython(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB),
		executor.NewNode(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB),
		executor.NewC(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB, compileLimits),
		executor.NewCpp(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB, compileLimits),
	)

	srv := httpapi.New(svc, httpapi.Config{
		Addr:              cfg.Addr,
		Token:             cfg.ServiceToken,
		MaxSourceBytes:    cfg.MaxSourceBytes,
		MaxConcurrentRuns: cfg.MaxConcurrentRuns,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
