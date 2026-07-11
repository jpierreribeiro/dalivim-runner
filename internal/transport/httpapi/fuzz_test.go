package httpapi

// S1 — fuzz the request decoder (see docs/future/security/S1-fuzzing-validators.md).
//
// handler.decode is the transport's coarse, language-agnostic gate: it bounds the
// body, base64-decodes the input streams when asked, and enforces the size caps
// BEFORE anything reaches the executor. FuzzDecode feeds it arbitrary request
// bodies and asserts two invariants: it never panics, and any request it ACCEPTS
// already satisfies every size cap it is responsible for. A crasher here is either
// a decoder panic (a DoS) or a cap that can be bypassed (a limit relaxation).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fuzzDecodeHandler is a decode-only handler with small, exercised caps. It needs
// none of the execution machinery — decode reads only the cap fields.
func fuzzDecodeHandler() *handler {
	return &handler{
		maxSourceBytes:     4096,
		maxStdinBytes:      1024,
		maxFilesBytes:      8192,
		maxBatch:           16,
		maxBatchStdinBytes: 4096,
	}
}

func FuzzDecode(f *testing.F) {
	seeds := []string{
		`{"language":"python","source_code":"print(1)"}`,
		`{"language":"python","stdin":"aGVsbG8=","encoding":"base64"}`,
		`{"language":"python","stdins":["YQ==","Yg=="],"encoding":"base64"}`,
		`{"language":"python","encoding":"base64","stdin":"!!!not base64!!!"}`,
		`{"language":"python","encoding":"weird"}`,
		`{`,
		``,
		`[]`,
		`{"stdin":"` + strings.Repeat("A", 4096) + `"}`,
		`{"stdins":["a","b","c"]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		h := fuzzDecodeHandler()
		r := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		req, ok := h.decode(w, r) // must never panic on any input
		if !ok {
			return // a rejected request carries no guarantees about req
		}
		// ACCEPTED ⇒ every cap decode is responsible for already holds on the
		// POST-decode (post-base64) values. A violation here means a size limit
		// could be bypassed through the decoder.
		if len(req.SourceCode) > h.maxSourceBytes {
			t.Fatalf("accepted source_code of %d bytes (cap %d)", len(req.SourceCode), h.maxSourceBytes)
		}
		if h.maxStdinBytes > 0 && len(req.Stdin) > h.maxStdinBytes {
			t.Fatalf("accepted stdin of %d bytes (cap %d)", len(req.Stdin), h.maxStdinBytes)
		}
		if n := len(req.Stdins); n > 0 {
			if h.maxBatch > 0 && n > h.maxBatch {
				t.Fatalf("accepted %d stdins (cap %d)", n, h.maxBatch)
			}
			total := 0
			for i, in := range req.Stdins {
				if h.maxStdinBytes > 0 && len(in) > h.maxStdinBytes {
					t.Fatalf("accepted stdins[%d] of %d bytes (cap %d)", i, len(in), h.maxStdinBytes)
				}
				total += len(in)
			}
			if h.maxBatchStdinBytes > 0 && total > h.maxBatchStdinBytes {
				t.Fatalf("accepted batch stdin total of %d bytes (cap %d)", total, h.maxBatchStdinBytes)
			}
		}
	})
}
