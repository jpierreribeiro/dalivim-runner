package executor

import (
	"bytes"
	"errors"
)

// errOutputLimit is the cancellation cause a run uses when it kills the child
// for flooding output. The run layer distinguishes it from a deadline
// (context.DeadlineExceeded) via context.Cause to classify the outcome as
// output_limit_exceeded rather than timeout.
var errOutputLimit = errors.New("output limit exceeded")

// limitedBuffer caps captured output so a print-loop in submitted code cannot
// exhaust the runner's memory: bytes past the limit are counted and discarded
// (never buffered), and a marker is appended once when truncation occurred.
//
// When onLimit is set, it is invoked exactly once — the moment the stream first
// crosses the cap — so the run layer can kill the process instead of letting it
// burn its whole CPU budget writing bytes that are already being discarded
// (G1.4). The already-captured prefix is preserved so the caller still sees the
// first cap bytes.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
	onLimit   func() // fired once on first overflow; nil disables the kill signal
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining <= 0 {
		l.markTruncated()
		return len(p), nil // discard, but do not error the pipe
	}
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.markTruncated()
		return len(p), nil
	}
	return l.buf.Write(p)
}

// markTruncated records the overflow and, on the first crossing only, fires the
// onLimit signal. Write for a single stream is called serially by the os/exec
// copy goroutine, so the once-guard needs no lock; onLimit itself (a context
// cancel) is safe to call from the two per-stream goroutines concurrently.
func (l *limitedBuffer) markTruncated() {
	if l.truncated {
		return
	}
	l.truncated = true
	if l.onLimit != nil {
		l.onLimit()
	}
}

func (l *limitedBuffer) String() string {
	s := l.buf.String()
	if l.truncated {
		s += "\n[output truncated]"
	}
	return s
}
