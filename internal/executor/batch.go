package executor

import (
	"context"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// batchLoop drives one batch (G6): it runs exec once per stdin, SEQUENTIALLY —
// parallel inputs would fight the global concurrency limiter and muddy per-run
// cgroup accounting — and index-aligned, so results[i] ran against stdins[i].
// Per-input failures are independent; only the batch-total wall budget stops
// the loop early (aborted=true, results a partial prefix).
//
// start is the batch epoch — taken BEFORE the compile phase, so a compile that
// eats the whole budget aborts before the first input rather than extending the
// batch past it. The budget is checked between runs, not mid-run: each run is
// already bounded by its own per-run timeout, so the worst overshoot is one
// per-run timeout past the budget. 0 disables the budget.
func batchLoop(ctx context.Context, stdins []string, start time.Time, totalBudgetMs int, exec func(ctx context.Context, stdin string) runnerapi.RunResult) (results []runnerapi.RunResult, aborted bool) {
	results = make([]runnerapi.RunResult, 0, len(stdins))
	for _, in := range stdins {
		if totalBudgetMs > 0 && time.Since(start) >= time.Duration(totalBudgetMs)*time.Millisecond {
			return results, true
		}
		if ctx.Err() != nil {
			// The caller (HTTP request) is gone or shutting down; stop cleanly with
			// what completed instead of burning the remaining inputs.
			return results, true
		}
		results = append(results, exec(ctx, in))
	}
	return results, false
}
