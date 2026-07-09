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
	CgroupPolicy  string // RUNNER_CGROUP: auto|require|off (empty => auto) — per-run cgroup v2 accounting
	CgroupMount   string // RUNNER_CGROUP_MOUNT: delegated writable cgroup v2 subtree for per-run leaves
	MaxProcesses  int    // per-run RLIMIT_NPROC applied by the nsjail backend (fork-bomb cap)
	MaxFileSizeMB int    // per-run RLIMIT_FSIZE applied by the nsjail backend

	MaxConcurrentRuns int // simultaneous executions before the runner sheds load with 503

	MetricsToken        string // RUNNER_METRICS_TOKEN: gates GET /metrics; empty => the service token
	ReadyRequiresNsjail bool   // RUNNER_READY_REQUIRES: /readyz needs nsjail active (default true)

	DefaultTimeoutMs int
	MaxTimeoutMs     int
	DefaultMemoryMB  int
	MaxMemoryMB      int
	MaxSourceBytes   int
	MaxStdinBytes    int // per-request stdin cap, independent of the source budget
	MaxOutputBytes   int

	// Multi-file submission caps (G3). These bound the attacker-controlled
	// files[] payload: how many files, how large each is, the summed budget, and
	// how long/deep a single path may be. Every one is enforced before a byte
	// touches disk (see executor.FilePolicy).
	MaxFiles      int // most files a single request may carry
	MaxFileBytes  int // largest a single file's content may be
	MaxFilesBytes int // largest the summed content of all files may be
	MaxPathBytes  int // longest a single file path may be
	MaxPathDepth  int // deepest a path may nest (number of components)

	// Compiled-language (C/C++) compile phase — separate budget from execution.
	CompileTimeoutMs    int // default per-run compile wall/CPU budget
	MaxCompileTimeoutMs int // hard ceiling for compile time
	CompileMemoryMB     int // RLIMIT_AS / cgroup for the compiler (bombs)
	MaxArtifactBytes    int // reject a compiled artifact larger than this

	// StaticSeccomp: off|enforce|complain (empty => off) — the seccomp profile for
	// the compiled RUN jail. off keeps the shared denylist; enforce installs the
	// tight static-binary allowlist (SIGSYS on anything unlisted); complain logs
	// violations without killing, for tuning the set on the target.
	StaticSeccomp string
}

// Load reads and validates configuration. It returns an error (rather than
// exiting) so the caller owns process lifecycle.
func Load() (Config, error) {
	cfg := Config{
		Addr:                ":" + port(),
		ServiceToken:        os.Getenv("RUNNER_SERVICE_TOKEN"),
		Development:         strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_ENV")), "development"),
		SandboxPolicy:       os.Getenv("RUNNER_SANDBOX"),
		NetworkPolicy:       os.Getenv("RUNNER_NETWORK_ISOLATION"),
		CgroupPolicy:        os.Getenv("RUNNER_CGROUP"),
		CgroupMount:         cgroupMount(),
		MaxProcesses:        envInt("RUNNER_MAX_PROCESSES", 256),
		MaxFileSizeMB:       envInt("RUNNER_MAX_FILE_SIZE_MB", 64),
		MaxConcurrentRuns:   envInt("RUNNER_MAX_CONCURRENT_RUNS", 8),
		MetricsToken:        os.Getenv("RUNNER_METRICS_TOKEN"),
		ReadyRequiresNsjail: readyRequiresNsjail(),
		DefaultTimeoutMs:    envInt("RUNNER_DEFAULT_TIMEOUT_MS", 3000),
		MaxTimeoutMs:        envInt("RUNNER_MAX_TIMEOUT_MS", 10000),
		DefaultMemoryMB:     envInt("RUNNER_DEFAULT_MEMORY_MB", 128),
		MaxMemoryMB:         envInt("RUNNER_MAX_MEMORY_MB", 512),
		MaxSourceBytes:      envInt("RUNNER_MAX_SOURCE_BYTES", 200_000),
		MaxStdinBytes:       envInt("RUNNER_MAX_STDIN_BYTES", 1_000_000),
		MaxOutputBytes:      envInt("RUNNER_MAX_OUTPUT_BYTES", 64*1024),

		MaxFiles:      envInt("RUNNER_MAX_FILES", 50),
		MaxFileBytes:  envInt("RUNNER_MAX_FILE_BYTES", 262_144),
		MaxFilesBytes: envInt("RUNNER_MAX_FILES_BYTES", 1_048_576),
		MaxPathBytes:  envInt("RUNNER_MAX_PATH_BYTES", 180),
		MaxPathDepth:  envInt("RUNNER_MAX_PATH_DEPTH", 8),

		CompileTimeoutMs:    envInt("RUNNER_COMPILE_TIMEOUT_MS", 10_000),
		MaxCompileTimeoutMs: envInt("RUNNER_MAX_COMPILE_TIMEOUT_MS", 20_000),
		CompileMemoryMB:     envInt("RUNNER_COMPILE_MEMORY_MB", 512),
		MaxArtifactBytes:    envInt("RUNNER_MAX_ARTIFACT_MB", 32) * 1024 * 1024,
		StaticSeccomp:       os.Getenv("RUNNER_STATIC_SECCOMP"),
	}
	if cfg.ServiceToken == "" && !cfg.Development {
		return Config{}, fmt.Errorf("RUNNER_SERVICE_TOKEN is required outside development; set it (and send X-Runner-Token from the gateway) or set RUNNER_ENV=development for local use")
	}
	return cfg, nil
}

// cgroupMount resolves the delegated cgroup v2 subtree for per-run accounting,
// defaulting to /sys/fs/cgroup/dalivim. It is only consulted by the nsjail
// backend under RUNNER_CGROUP=auto|require; when the path is absent or not
// delegated, "auto" simply falls back to rlimit bounds (see resolveCgroup).
func cgroupMount() string {
	if m := strings.TrimSpace(os.Getenv("RUNNER_CGROUP_MOUNT")); m != "" {
		return m
	}
	return "/sys/fs/cgroup/dalivim"
}

// readyRequiresNsjail resolves RUNNER_READY_REQUIRES for the /readyz threshold.
// Default (unset) and "nsjail" require the nsjail backend to be active for readiness;
// "none"/"off"/"any" relax it (always ready when the process is up), for operators
// intentionally running the netns-only backend.
func readyRequiresNsjail() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RUNNER_READY_REQUIRES"))) {
	case "none", "off", "any":
		return false
	default: // "" (default) or "nsjail"
		return true
	}
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
