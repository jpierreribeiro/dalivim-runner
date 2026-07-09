package executor

import (
	"strings"
	"testing"
)

// TestLimitedBuffer_TruncatesAndSignalsOnce pins G1.4's buffer contract: bytes
// past the cap are discarded, the marker is appended once, and onLimit fires
// exactly once on the first overflow (so the run layer kills the child once, not
// per write).
func TestLimitedBuffer_TruncatesAndSignalsOnce(t *testing.T) {
	fired := 0
	b := &limitedBuffer{limit: 8, onLimit: func() { fired++ }}

	// First write stays under the cap: no truncation, no signal.
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatalf("write under cap: %v", err)
	}
	if b.truncated || fired != 0 {
		t.Fatalf("under cap must not truncate/signal (truncated=%v fired=%d)", b.truncated, fired)
	}

	// Second write crosses the cap: keep the prefix up to the limit, signal once.
	if _, err := b.Write([]byte("567890")); err != nil {
		t.Fatalf("write over cap: %v", err)
	}
	// Third write is entirely past the cap: discarded, no second signal.
	if _, err := b.Write([]byte("more")); err != nil {
		t.Fatalf("write past cap: %v", err)
	}
	if fired != 1 {
		t.Fatalf("onLimit must fire exactly once, fired %d", fired)
	}
	got := b.String()
	if !strings.HasSuffix(got, "\n[output truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if body := strings.TrimSuffix(got, "\n[output truncated]"); body != "12345678" {
		t.Fatalf("expected first 8 bytes captured, got %q", body)
	}
}

// TestLimitedBuffer_NoSignalWhenNil confirms a buffer without onLimit (the compile
// phase) still truncates but never panics on the missing callback.
func TestLimitedBuffer_NoSignalWhenNil(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	if _, err := b.Write([]byte("overflowing")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !b.truncated {
		t.Fatal("expected truncation")
	}
}
