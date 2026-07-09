package executor

import "bytes"

// limitedBuffer caps captured output so a print-loop in submitted code cannot
// exhaust the runner's memory: bytes past the limit are counted and discarded
// (never buffered), and a marker is appended once when truncation occurred.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining <= 0 {
		l.truncated = true
		return len(p), nil // discard, but do not error the pipe
	}
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.truncated = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) String() string {
	s := l.buf.String()
	if l.truncated {
		s += "\n[output truncated]"
	}
	return s
}
