package config

import (
	"reflect"
	"testing"
)

// TestTokenSet_MergesDedupsAndCaps pins G8.2's parsing: the singular and plural
// sources merge into one order-preserving, deduplicated set; blanks are dropped;
// and the set is capped at maxTokens.
func TestTokenSet_MergesDedupsAndCaps(t *testing.T) {
	got := tokenSet("primary", "a, b  c,,primary")
	want := []string{"primary", "a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenSet merge/dedup = %v, want %v", got, want)
	}

	if s := tokenSet("", "   "); len(s) != 0 {
		t.Fatalf("blank sources must yield an empty set, got %v", s)
	}

	many := make([]byte, 0)
	sources := ""
	for i := 0; i < maxTokens+5; i++ {
		if i > 0 {
			sources += ","
		}
		sources += string(append(many, byte('a'+i)))
	}
	if capped := tokenSet(sources); len(capped) != maxTokens {
		t.Fatalf("token set must cap at %d, got %d", maxTokens, len(capped))
	}
}

// TestShutdownGrace_RespectsInvariant pins G8.3: the grace floor outlasts the
// worst-case request (compile + run + slack), and the env override can only
// raise it, never drop it below the floor.
func TestShutdownGrace_RespectsInvariant(t *testing.T) {
	const compile, run = 20_000, 10_000
	// A small batch budget keeps the single run (compile+run) the worst case.
	const smallBatch = 5_000
	floor := compile + run + shutdownGraceSlackMs

	if got := shutdownGraceMs(compile, run, smallBatch, 0); got != floor {
		t.Fatalf("derived grace = %d, want floor %d", got, floor)
	}

	t.Setenv("RUNNER_SHUTDOWN_GRACE_MS", "1000") // below the floor
	if got := shutdownGraceMs(compile, run, smallBatch, 0); got != floor {
		t.Fatalf("an override below the floor must be ignored: got %d, want %d", got, floor)
	}

	t.Setenv("RUNNER_SHUTDOWN_GRACE_MS", "99000") // above the floor
	if got := shutdownGraceMs(compile, run, smallBatch, 0); got != 99_000 {
		t.Fatalf("an override above the floor must win: got %d, want 99000", got)
	}
}

// TestShutdownGrace_CoversBatchBudget pins the batch half of the G8.3 invariant:
// when the batch-wide wall budget (plus one per-run timeout of between-runs
// overshoot) exceeds a single compile+run, the floor tracks the BATCH so a deploy
// cannot kill an in-flight batch before its own budget elapses.
func TestShutdownGrace_CoversBatchBudget(t *testing.T) {
	const compile, run = 20_000, 10_000
	const bigBatch = 60_000 // the default; larger than compile+run (30_000)

	batchFloor := bigBatch + run + shutdownGraceSlackMs
	if got := shutdownGraceMs(compile, run, bigBatch, 0); got != batchFloor {
		t.Fatalf("grace must cover the batch budget: got %d, want %d", got, batchFloor)
	}
	// Sanity: the batch floor is strictly larger than the single-run floor it
	// would otherwise have used — i.e. the fix actually raised the guarantee.
	if singleFloor := compile + run + shutdownGraceSlackMs; batchFloor <= singleFloor {
		t.Fatalf("batch floor %d must exceed single-run floor %d", batchFloor, singleFloor)
	}
}

// TestShutdownGrace_CoversTestBudget pins the G9 half of the invariant: a
// mode=test run holds one slot for up to MaxTestTimeoutMs, so when that exceeds
// both a single compile+run and the batch budget, the grace floor must track the
// test budget — else a deploy could kill an in-flight test suite mid-run.
func TestShutdownGrace_CoversTestBudget(t *testing.T) {
	const compile, run = 20_000, 10_000
	const smallBatch = 5_000
	const bigTest = 90_000 // larger than compile+run and the batch budget

	testFloor := bigTest + shutdownGraceSlackMs
	if got := shutdownGraceMs(compile, run, smallBatch, bigTest); got != testFloor {
		t.Fatalf("grace must cover the test budget: got %d, want %d", got, testFloor)
	}
}

// TestLoad_RequiresTokenOutsideDev pins the fail-closed rule: no service token
// outside development is a hard error; a plural-only token or development mode
// both satisfy it.
func TestLoad_RequiresTokenOutsideDev(t *testing.T) {
	t.Setenv("RUNNER_SERVICE_TOKEN", "")
	t.Setenv("RUNNER_SERVICE_TOKENS", "")
	t.Setenv("RUNNER_ENV", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail closed when no token is set outside development")
	}

	t.Setenv("RUNNER_SERVICE_TOKENS", "t1, t2")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a plural-only token set must satisfy the requirement: %v", err)
	}
	if cfg.ServiceToken != "t1" || len(cfg.ServiceTokens) != 2 {
		t.Fatalf("primary/set not resolved from RUNNER_SERVICE_TOKENS: %q %v", cfg.ServiceToken, cfg.ServiceTokens)
	}

	t.Setenv("RUNNER_SERVICE_TOKENS", "")
	t.Setenv("RUNNER_ENV", "development")
	if _, err := Load(); err != nil {
		t.Fatalf("development must permit an empty token set: %v", err)
	}
}

// TestLoad_CgroupDefaultIsStrictInProduction pins RUN-02: an omitted cgroup
// policy must not silently select auto for runtimes that cannot use RLIMIT_AS.
// Development retains the convenient auto default, and an explicit production
// auto remains an operator-visible availability tradeoff.
func TestLoad_CgroupDefaultIsStrictInProduction(t *testing.T) {
	t.Setenv("RUNNER_SERVICE_TOKEN", "test-token")
	t.Setenv("RUNNER_SERVICE_TOKENS", "")
	t.Setenv("RUNNER_ENV", "")
	t.Setenv("RUNNER_CGROUP", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CgroupPolicy != "require" {
		t.Fatalf("production empty RUNNER_CGROUP = %q, want require", cfg.CgroupPolicy)
	}

	t.Setenv("RUNNER_ENV", "development")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CgroupPolicy != "" {
		t.Fatalf("development empty RUNNER_CGROUP = %q, want empty (auto)", cfg.CgroupPolicy)
	}

	t.Setenv("RUNNER_ENV", "")
	t.Setenv("RUNNER_CGROUP", "auto")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CgroupPolicy != "auto" {
		t.Fatalf("explicit production RUNNER_CGROUP must be preserved, got %q", cfg.CgroupPolicy)
	}
}
