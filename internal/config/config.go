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
	Addr         string // listen address, e.g. ":8090"
	ServiceToken string // primary shared secret (RUNNER_SERVICE_TOKEN); callers send it as X-Runner-Token
	// ServiceTokens is the full set of currently-valid service tokens: the
	// primary RUNNER_SERVICE_TOKEN plus any in RUNNER_SERVICE_TOKENS (comma/space
	// separated). A request authorizes if it matches ANY (each compared in
	// constant time), which makes rotation zero-downtime: add the new token, deploy,
	// point the backend at it, then drop the old one — no window where either is
	// rejected. Empty only in development (fail-closed in prod).
	ServiceTokens []string
	Development   bool   // RUNNER_ENV=development relaxes the token requirement
	SandboxPolicy string // RUNNER_SANDBOX: auto|require|off (empty => auto) — nsjail selection
	NetworkPolicy string // RUNNER_NETWORK_ISOLATION: auto|require|off (empty => auto) — netns backend
	CgroupPolicy  string // RUNNER_CGROUP: auto|require|off (empty => require in production, auto in development)
	CgroupMount   string // RUNNER_CGROUP_MOUNT: delegated writable cgroup v2 subtree for per-run leaves
	MaxProcesses  int    // per-run RLIMIT_NPROC applied by the nsjail backend (fork-bomb cap)
	MaxFileSizeMB int    // per-run RLIMIT_FSIZE applied by the nsjail backend

	MaxConcurrentRuns int // simultaneous executions before the runner sheds load with 503

	MetricsToken        string   // RUNNER_METRICS_TOKEN: gates GET /metrics; empty => the service token(s)
	MetricsTokens       []string // full valid set for /metrics: RUNNER_METRICS_TOKEN(S), or the service set when unset
	ReadyRequiresNsjail bool     // RUNNER_READY_REQUIRES: /readyz needs nsjail active (default true)

	DefaultTimeoutMs int
	MaxTimeoutMs     int
	DefaultMemoryMB  int
	MaxMemoryMB      int
	MaxSourceBytes   int
	MaxStdinBytes    int // per-request stdin cap, independent of the source budget
	MaxOutputBytes   int

	// Test-runner mode (G9). A mode=test request runs a whole test framework, which
	// legitimately takes longer than one program, so it clamps to a SEPARATE and
	// larger timeout envelope than a run; memory reuses the run-mode budget. The
	// report is capped independently of the stdout cap (a suite with thousands of
	// failures emits a large report).
	DefaultTestTimeoutMs int // default per-run budget for a mode=test request
	MaxTestTimeoutMs     int // hard ceiling for a mode=test run
	MaxTestReportBytes   int // cap on the returned test_report

	// Multi-file submission caps (G3). These bound the attacker-controlled
	// files[] payload: how many files, how large each is, the summed budget, and
	// how long/deep a single path may be. Every one is enforced before a byte
	// touches disk (see executor.FilePolicy).
	MaxFiles      int // most files a single request may carry
	MaxFileBytes  int // largest a single file's content may be
	MaxFilesBytes int // largest the summed content of all files may be
	MaxPathBytes  int // longest a single file path may be
	MaxPathDepth  int // deepest a path may nest (number of components)

	// Batch execution caps (G6). A stdins[] batch holds ONE concurrency slot for
	// its whole duration, so worst-case wall pressure is
	// MaxConcurrentRuns × MaxBatch × per-run timeout — these caps bound that
	// product. MaxBatchTotalMs is the batch-wide wall budget (compile included);
	// crossing it aborts the batch with partial results.
	MaxBatch           int // most stdins one request may carry
	MaxBatchTotalMs    int // batch-wide wall budget in ms
	MaxBatchStdinBytes int // summed stdin bytes across a batch

	// Compiled-language (C/C++) compile phase — separate budget from execution.
	CompileTimeoutMs    int // default per-run compile wall/CPU budget
	MaxCompileTimeoutMs int // hard ceiling for compile time
	CompileMemoryMB     int // RLIMIT_AS / cgroup for the compiler (bombs)
	MaxArtifactBytes    int // reject a compiled artifact larger than this

	// ShutdownGraceMs is how long the server drains in-flight runs on SIGTERM
	// before forcing them down (G8.3). It MUST be ≥ the longest a single request
	// can legitimately occupy the server — the greater of a single run (a compile
	// phase up to MaxCompileTimeoutMs followed by a run up to MaxTimeoutMs) and a
	// batch (the MaxBatchTotalMs wall budget plus one per-run timeout of
	// between-runs overshoot) — plus slack, or a deploy would kill a long run
	// mid-flight and surface a spurious failure to the student. Derived from those
	// ceilings; RUNNER_SHUTDOWN_GRACE_MS may RAISE it but never lower it below the
	// invariant.
	ShutdownGraceMs int

	// StaticSeccomp: off|enforce|complain (empty => off) — the seccomp profile for
	// the compiled RUN jail. off keeps the shared denylist; enforce installs the
	// tight static-binary allowlist (SIGSYS on anything unlisted); complain logs
	// violations without killing, for tuning the set on the target.
	StaticSeccomp string
}

// maxTokens caps how many valid tokens a set may carry. A rotation needs only
// two live at once (old + new); the cap keeps a fat-fingered env from turning
// the constant-time per-candidate compare into an unbounded loop.
const maxTokens = 8

// Load reads and validates configuration. It returns an error (rather than
// exiting) so the caller owns process lifecycle.
func Load() (Config, error) {
	serviceTokens := tokenSet(os.Getenv("RUNNER_SERVICE_TOKEN"), os.Getenv("RUNNER_SERVICE_TOKENS"))
	development := strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_ENV")), "development")
	primary := ""
	if len(serviceTokens) > 0 {
		primary = serviceTokens[0]
	}
	cfg := Config{
		Addr:                ":" + port(),
		ServiceToken:        primary,
		ServiceTokens:       serviceTokens,
		Development:         development,
		SandboxPolicy:       os.Getenv("RUNNER_SANDBOX"),
		NetworkPolicy:       os.Getenv("RUNNER_NETWORK_ISOLATION"),
		CgroupPolicy:        cgroupPolicy(development),
		CgroupMount:         cgroupMount(),
		MaxProcesses:        envInt("RUNNER_MAX_PROCESSES", 256),
		MaxFileSizeMB:       envInt("RUNNER_MAX_FILE_SIZE_MB", 64),
		MaxConcurrentRuns:   envInt("RUNNER_MAX_CONCURRENT_RUNS", 8),
		MetricsToken:        os.Getenv("RUNNER_METRICS_TOKEN"),
		MetricsTokens:       tokenSet(os.Getenv("RUNNER_METRICS_TOKEN"), os.Getenv("RUNNER_METRICS_TOKENS")),
		ReadyRequiresNsjail: readyRequiresNsjail(),
		DefaultTimeoutMs:    envInt("RUNNER_DEFAULT_TIMEOUT_MS", 3000),
		MaxTimeoutMs:        envInt("RUNNER_MAX_TIMEOUT_MS", 10000),
		DefaultMemoryMB:     envInt("RUNNER_DEFAULT_MEMORY_MB", 128),
		MaxMemoryMB:         envInt("RUNNER_MAX_MEMORY_MB", 512),
		MaxSourceBytes:      envInt("RUNNER_MAX_SOURCE_BYTES", 200_000),
		MaxStdinBytes:       envInt("RUNNER_MAX_STDIN_BYTES", 1_000_000),
		MaxOutputBytes:      envInt("RUNNER_MAX_OUTPUT_BYTES", 64*1024),

		DefaultTestTimeoutMs: envInt("RUNNER_TEST_TIMEOUT_MS", 15_000),
		MaxTestTimeoutMs:     envInt("RUNNER_MAX_TEST_TIMEOUT_MS", 30_000),
		MaxTestReportBytes:   envInt("RUNNER_MAX_TEST_REPORT_BYTES", 4_000_000),

		MaxFiles:      envInt("RUNNER_MAX_FILES", 50),
		MaxFileBytes:  envInt("RUNNER_MAX_FILE_BYTES", 262_144),
		MaxFilesBytes: envInt("RUNNER_MAX_FILES_BYTES", 1_048_576),
		MaxPathBytes:  envInt("RUNNER_MAX_PATH_BYTES", 180),
		MaxPathDepth:  envInt("RUNNER_MAX_PATH_DEPTH", 8),

		MaxBatch:           envInt("RUNNER_MAX_BATCH", 100),
		MaxBatchTotalMs:    envInt("RUNNER_MAX_BATCH_TOTAL_MS", 60_000),
		MaxBatchStdinBytes: envInt("RUNNER_MAX_BATCH_STDIN_BYTES", 4_000_000),

		CompileTimeoutMs:    envInt("RUNNER_COMPILE_TIMEOUT_MS", 10_000),
		MaxCompileTimeoutMs: envInt("RUNNER_MAX_COMPILE_TIMEOUT_MS", 20_000),
		CompileMemoryMB:     envInt("RUNNER_COMPILE_MEMORY_MB", 512),
		MaxArtifactBytes:    envInt("RUNNER_MAX_ARTIFACT_MB", 32) * 1024 * 1024,
		StaticSeccomp:       os.Getenv("RUNNER_STATIC_SECCOMP"),
	}
	// G8.3 invariant: the drain grace must outlast the worst-case in-flight request
	// — the greater of a single run (full compile + full run budget) and a batch
	// (the whole MaxBatchTotalMs wall budget) — plus slack, so a deploy never kills
	// a request that is still inside its own deadline. RUNNER_SHUTDOWN_GRACE_MS can
	// only raise it above this floor.
	cfg.ShutdownGraceMs = shutdownGraceMs(cfg.MaxCompileTimeoutMs, cfg.MaxTimeoutMs, cfg.MaxBatchTotalMs, cfg.MaxTestTimeoutMs)
	if len(cfg.ServiceTokens) == 0 && !cfg.Development {
		return Config{}, fmt.Errorf("RUNNER_SERVICE_TOKEN(S) is required outside development; set RUNNER_SERVICE_TOKEN (or a comma/space-separated RUNNER_SERVICE_TOKENS for zero-downtime rotation), and send X-Runner-Token from the gateway, or set RUNNER_ENV=development for local use")
	}
	return cfg, nil
}

// cgroupPolicy makes an omitted RUNNER_CGROUP fail closed outside development.
// Several enabled runtimes (Go, JavaScript, Java and TypeScript) cannot tolerate
// RLIMIT_AS, so the former implicit auto fallback could leave them without a
// per-run RSS ceiling. Operators may still choose auto explicitly as an
// availability tradeoff; an omission in production is never that choice.
func cgroupPolicy(development bool) string {
	policy := strings.TrimSpace(os.Getenv("RUNNER_CGROUP"))
	if policy == "" && !development {
		return "require"
	}
	return policy
}

// tokenSet parses one or more env sources into a deduplicated, order-preserving
// set of valid tokens. Each source may itself be comma/space-separated
// (RUNNER_SERVICE_TOKENS), so a single primary var and a plural rotation var
// merge into one set. Blank entries are dropped; the result is capped at
// maxTokens (extra entries are ignored, never silently authorizing more).
func tokenSet(sources ...string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, s := range sources {
		for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			if tok == "" {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
			if len(out) == maxTokens {
				return out
			}
		}
	}
	return out
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

// shutdownGraceSlackMs is the fixed headroom added over the worst-case request
// wall-time: time for the run to observe its own deadline, be reaped, and the
// response to flush before the drain deadline fires.
const shutdownGraceSlackMs = 5000

// shutdownGraceMs computes the drain grace as the invariant floor, then lets
// RUNNER_SHUTDOWN_GRACE_MS raise it — never lower it below the floor, so the
// G8.3 guarantee holds regardless of the override.
//
// The floor must outlast the LONGEST a single request can legitimately hold a
// concurrency slot, so a deploy SIGTERM never kills a request inside its own
// deadline. Two shapes compete for "longest":
//   - a single run: full compile budget + full run budget;
//   - a batch (stdins[]): the batch-wide wall budget, which batchLoop may
//     overshoot by at most one per-run timeout (the budget is checked BETWEEN
//     runs, never mid-run). A batch holds one slot for its whole duration, so
//     with a large MaxBatchTotalMs it — not the single run — is the worst case.
//
// A third shape competes for "longest": a mode=test run (G9) holds one slot for
// up to MaxTestTimeoutMs — larger than a run-mode timeout — so it is folded into
// the max as well.
//
// The floor is the max of the three, plus slack.
func shutdownGraceMs(maxCompileMs, maxRunMs, maxBatchTotalMs, maxTestMs int) int {
	worst := maxCompileMs + maxRunMs
	if batchWorst := maxBatchTotalMs + maxRunMs; batchWorst > worst {
		worst = batchWorst
	}
	if maxTestMs > worst {
		worst = maxTestMs
	}
	floor := worst + shutdownGraceSlackMs
	if v := envInt("RUNNER_SHUTDOWN_GRACE_MS", floor); v > floor {
		return v
	}
	return floor
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
