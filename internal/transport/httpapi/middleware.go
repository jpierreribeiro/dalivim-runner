package httpapi

import (
	"crypto/subtle"
	"net/http"

	"github.com/jpierreribeiro/dalivim-runner/internal/metrics"
)

// RequireTokens is the zero-trust gate for this internal service. Because /run
// is a remote-code-execution surface, every request must present a valid
// pre-shared service token as the X-Runner-Token header; only the main API,
// which holds one of the same secrets, can dispatch executions.
//
// A request authorizes if its header matches ANY token in the set. This is what
// makes rotation zero-downtime: run the old and new tokens together, cut the
// backend over, then retire the old one — no window where a valid caller is
// rejected. Every candidate is compared with a constant-time compare and the
// loop does NOT early-exit on the first match, so neither which token matched
// nor how many were tried leaks through response timing.
//
// When the set is empty the gate is a pass-through — reachable only in
// development, where config.Load permits an empty set; in production startup
// fails closed if no token is set.
func RequireTokens(tokens []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(tokens) > 0 {
			got := []byte(r.Header.Get("X-Runner-Token"))
			ok := 0
			for _, t := range tokens {
				// Bitwise-OR the results so all candidates are always compared —
				// no data-dependent early exit that could leak timing.
				ok |= subtle.ConstantTimeCompare(got, []byte(t))
			}
			if ok != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// LimitConcurrency bounds how many executions run at once. Each run is CPU- and
// memory-hungry, so unbounded concurrency lets a burst exhaust the container and
// take the service down. When every slot is taken the limiter fails fast with
// 503 (and Retry-After) instead of queueing, so the caller learns immediately
// and can shed load or fall back to another backend. Placed INSIDE RequireToken
// so only authenticated callers can consume a slot — an unauthenticated flood
// cannot starve the gateway of capacity.
//
// max <= 0 disables the limit (pass-through). m records the inflight gauge and
// the overload counter (nil-safe).
func LimitConcurrency(max int, m *metrics.Metrics, next http.Handler) http.Handler {
	if max <= 0 {
		return next
	}
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			m.IncInflight()
			defer func() { <-slots; m.DecInflight() }()
			next.ServeHTTP(w, r)
		default:
			m.Overload()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "runner at capacity", http.StatusServiceUnavailable)
		}
	})
}
