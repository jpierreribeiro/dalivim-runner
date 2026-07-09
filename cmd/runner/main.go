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
	"strings"
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
	sb, err := sandbox.Configure(cfg.SandboxPolicy, cfg.NetworkPolicy, cfg.CgroupPolicy, cfg.CgroupMount)
	if err != nil {
		slog.Error("sandbox", "err", err)
		os.Exit(1)
	}

	// Interpreted runtimes share one sandbox: each plugs in as a languageSpec entry
	// plus a constructor, and inherits the identical jail. Compiled languages
	// (F-D) register the same way once their compile-phase runtime exists.
	svc := executor.NewService(
		executor.Limits{
			DefaultTimeout:        cfg.DefaultTimeoutMs,
			MaxTimeoutMs:          cfg.MaxTimeoutMs,
			DefaultMemory:         cfg.DefaultMemoryMB,
			MaxMemoryMB:           cfg.MaxMemoryMB,
			DefaultCompileTimeout: cfg.CompileTimeoutMs,
			MaxCompileTimeoutMs:   cfg.MaxCompileTimeoutMs,
			Files: executor.FileCaps{
				MaxFiles:      cfg.MaxFiles,
				MaxFileBytes:  cfg.MaxFileBytes,
				MaxFilesBytes: cfg.MaxFilesBytes,
				MaxPathBytes:  cfg.MaxPathBytes,
				MaxPathDepth:  cfg.MaxPathDepth,
			},
		},
		executor.NewPython(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB),
		executor.NewNode(sb, cfg.MaxOutputBytes, cfg.MaxProcesses, cfg.MaxFileSizeMB),
		executor.NewC(sb, compiledConfig(cfg)),
		executor.NewCpp(sb, compiledConfig(cfg)),
		executor.NewGo(sb, compiledConfig(cfg)),
		executor.NewJava(sb, compiledConfig(cfg)),
	)

	srv := httpapi.New(svc, httpapi.Config{
		Addr:                cfg.Addr,
		Token:               cfg.ServiceToken,
		MaxSourceBytes:      cfg.MaxSourceBytes,
		MaxStdinBytes:       cfg.MaxStdinBytes,
		MaxFilesBytes:       cfg.MaxFilesBytes,
		MaxConcurrentRuns:   cfg.MaxConcurrentRuns,
		MetricsToken:        cfg.MetricsToken,
		Backend:             sb.Backend(),
		NetworkIsolated:     sb.NetworkIsolated(),
		ReadyRequiresNsjail: cfg.ReadyRequiresNsjail,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

// compiledConfig gathers the compile-phase knobs for the C/C++ runtimes.
func compiledConfig(cfg config.Config) executor.CompiledConfig {
	return executor.CompiledConfig{
		OutputLimit:      cfg.MaxOutputBytes,
		MaxProcesses:     cfg.MaxProcesses,
		MaxFileSizeMB:    cfg.MaxFileSizeMB,
		CompileMemoryMB:  cfg.CompileMemoryMB,
		MaxArtifactBytes: cfg.MaxArtifactBytes,
		RunSeccomp:       staticSeccompProfile(cfg.StaticSeccomp),
	}
}

// staticSeccompProfile maps RUNNER_STATIC_SECCOMP (off|enforce|complain, empty =>
// off) to the run-jail seccomp profile for compiled languages, logging the choice.
// An unrecognised value degrades to the safe default (the shared denylist) rather
// than failing boot — the allowlist is an opt-in tightening.
func staticSeccompProfile(mode string) sandbox.SeccompProfile {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "enforce":
		slog.Info("compiled run jail: static seccomp ALLOWLIST enforced (DEFAULT KILL)")
		return sandbox.SeccompStaticEnforce
	case "complain":
		slog.Warn("compiled run jail: static seccomp allowlist in COMPLAIN mode (DEFAULT LOG) — violations are logged, NOT killed; for tuning only")
		return sandbox.SeccompStaticComplain
	default:
		return sandbox.SeccompDenylist
	}
}
