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
	floor := compile + run + shutdownGraceSlackMs

	if got := shutdownGraceMs(compile, run); got != floor {
		t.Fatalf("derived grace = %d, want floor %d", got, floor)
	}

	t.Setenv("RUNNER_SHUTDOWN_GRACE_MS", "1000") // below the floor
	if got := shutdownGraceMs(compile, run); got != floor {
		t.Fatalf("an override below the floor must be ignored: got %d, want %d", got, floor)
	}

	t.Setenv("RUNNER_SHUTDOWN_GRACE_MS", "99000") // above the floor
	if got := shutdownGraceMs(compile, run); got != 99_000 {
		t.Fatalf("an override above the floor must win: got %d, want 99000", got)
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
