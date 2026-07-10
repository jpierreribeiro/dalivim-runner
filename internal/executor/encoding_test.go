package executor

import (
	"encoding/base64"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

func TestEncodeStream(t *testing.T) {
	raw := "\x00\xff\xfeA" // includes invalid UTF-8 bytes
	if got := encodeStream("", raw); got != raw {
		t.Fatalf("default must pass through unchanged: got %q", got)
	}
	if got := encodeStream("utf8", raw); got != raw {
		t.Fatalf("utf8 must pass through unchanged: got %q", got)
	}
	want := base64.StdEncoding.EncodeToString([]byte(raw))
	if got := encodeStream("base64", raw); got != want {
		t.Fatalf("base64: got %q want %q", got, want)
	}
	if got := encodeStream(" BASE64 ", raw); got != want {
		t.Fatalf("base64 must be case/space-insensitive: got %q want %q", got, want)
	}
}

// TestBase64_BinaryOutputRoundTrips proves a program emitting invalid UTF-8 bytes
// survives with encoding=base64: the executor base64-encodes the raw process
// bytes, so decoding the response stdout yields them exactly. (Without base64 the
// executor still holds the raw bytes, but they are replaced with U+FFFD at the JSON
// boundary — proven end-to-end in the httpapi handler test.)
func TestBase64_BinaryOutputRoundTrips(t *testing.T) {
	requirePython(t)
	binary := []byte{0x00, 0xff, 0xfe, 0x41, 0x0a}
	res := run(t, newPython(t), runnerapi.RunRequest{
		SourceCode: "import sys; sys.stdout.buffer.write(bytes([0,255,254,65,10]))",
		Encoding:   "base64",
		TimeoutMs:  3000, MemoryMB: 128,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	got, err := base64.StdEncoding.DecodeString(res.Stdout)
	if err != nil {
		t.Fatalf("stdout is not valid base64: %q (%v)", res.Stdout, err)
	}
	if string(got) != string(binary) {
		t.Fatalf("binary output mismatch: got % x want % x", got, binary)
	}
}
