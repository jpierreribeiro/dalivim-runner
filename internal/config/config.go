// Package config reads and validates the service configuration from the
// environment exactly once, at startup. Every field has a safe default except
// the service token, which is required outside development so the runner fails
// closed rather than accepting anonymous callers.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the fully-resolved service configuration.
type Config struct {
	Addr          string // listen address, e.g. ":8090"
	ServiceToken  string // shared secret; callers send it as X-Runner-Token
	Development   bool   // RUNNER_ENV=development relaxes the token requirement
	SandboxPolicy string // RUNNER_SANDBOX: auto|require|off (empty => auto) — nsjail selection
	NetworkPolicy string // RUNNER_NETWORK_ISOLATION: auto|require|off (empty => auto) — netns backend
	MaxProcesses  int    // per-run RLIMIT_NPROC applied by the nsjail backend (fork-bomb cap)
	MaxFileSizeMB int    // per-run RLIMIT_FSIZE applied by the nsjail backend

	MaxConcurrentRuns int // simultaneous executions before the runner sheds load with 503

	DefaultTimeoutMs int
	MaxTimeoutMs     int
	DefaultMemoryMB  int
	MaxMemoryMB      int
	MaxSourceBytes   int
	MaxOutputBytes   int
}

// Load reads and validates configuration. It returns an error (rather than
// exiting) so the caller owns process lifecycle.
func Load() (Config, error) {
	cfg := Config{
		Addr:              ":" + port(),
		ServiceToken:      os.Getenv("RUNNER_SERVICE_TOKEN"),
		Development:       strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_ENV")), "development"),
		SandboxPolicy:     os.Getenv("RUNNER_SANDBOX"),
		NetworkPolicy:     os.Getenv("RUNNER_NETWORK_ISOLATION"),
		MaxProcesses:      envInt("RUNNER_MAX_PROCESSES", 256),
		MaxFileSizeMB:     envInt("RUNNER_MAX_FILE_SIZE_MB", 64),
		MaxConcurrentRuns: envInt("RUNNER_MAX_CONCURRENT_RUNS", 8),
		DefaultTimeoutMs:  envInt("RUNNER_DEFAULT_TIMEOUT_MS", 3000),
		MaxTimeoutMs:      envInt("RUNNER_MAX_TIMEOUT_MS", 10000),
		DefaultMemoryMB:   envInt("RUNNER_DEFAULT_MEMORY_MB", 128),
		MaxMemoryMB:       envInt("RUNNER_MAX_MEMORY_MB", 512),
		MaxSourceBytes:    envInt("RUNNER_MAX_SOURCE_BYTES", 200_000),
		MaxOutputBytes:    envInt("RUNNER_MAX_OUTPUT_BYTES", 64*1024),
	}
	if cfg.ServiceToken == "" && !cfg.Development {
		return Config{}, fmt.Errorf("RUNNER_SERVICE_TOKEN is required outside development; set it (and send X-Runner-Token from the gateway) or set RUNNER_ENV=development for local use")
	}
	return cfg, nil
}

// port resolves the listen port: RUNNER_PORT, else the platform-injected PORT
// (Railway/Heroku inject it), else 8090.
func port() string {
	if p := os.Getenv("RUNNER_PORT"); p != "" {
		return p
	}
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8090"
}

// envInt reads a positive integer env var, falling back to def on unset/invalid.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
