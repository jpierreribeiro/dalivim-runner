package executor

import (
	"encoding/base64"
	"strings"
)

// wantBase64 reports whether a request asked for binary-safe base64 I/O
// (RunRequest.Encoding == "base64", case-insensitive).
func wantBase64(encoding string) bool {
	return strings.EqualFold(strings.TrimSpace(encoding), "base64")
}

// encodeStream renders a captured output stream (stdout/stderr) for the wire.
//
// With base64 I/O the raw process bytes are base64-encoded, so arbitrary or binary
// output round-trips intact instead of being mangled by the U+FFFD replacement the
// JSON boundary applies to invalid UTF-8. The default (text) path returns the bytes
// unchanged, exactly as before. The captured content is read via limitedBuffer's
// String(), which preserves the bytes verbatim (a Go string is an arbitrary byte
// sequence) plus any truncation marker.
func encodeStream(encoding, s string) string {
	if wantBase64(encoding) {
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	return s
}
