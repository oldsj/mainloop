package httpx_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/httpx"
)

// jsonBody answers with n bytes of JSON in one write, as the generated handlers do.
func jsonBody(n int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bigJSON(n))
	})
}

func bigJSON(n int) []byte {
	return []byte(`"` + strings.Repeat("a", n-2) + `"`)
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return out
}

var acceptGzip = map[string]string{"Accept-Encoding": "gzip, deflate, br"}

func TestCompressGzipsALargeJSONBody(t *testing.T) {
	// Given a JSON answer the size of a long task list.
	h := httpx.Compress(jsonBody(200_000))

	// When a browser that accepts gzip asks for it.
	rec := do(h, "GET", "/tasks", acceptGzip)

	// Then it arrives gzipped, far smaller, and unpacks to the same bytes.
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want none: the length is no longer the handler's", got)
	}
	if rec.Body.Len() > 20_000 {
		t.Errorf("compressed body is %d bytes", rec.Body.Len())
	}
	if got := gunzip(t, rec.Body.Bytes()); !bytes.Equal(got, bigJSON(200_000)) {
		t.Errorf("unpacked body differs: %d bytes", len(got))
	}
}

func TestCompressKeepsTheHandlersStatus(t *testing.T) {
	// Given a large JSON refusal.
	h := httpx.Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(bigJSON(4096))
	}))

	// When it is compressed.
	rec := do(h, "GET", "/x", acceptGzip)

	// Then the status is still the handler's.
	if rec.Code != http.StatusConflict || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("status = %d, encoding = %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

func TestCompressLeavesOtherResponsesAsWritten(t *testing.T) {
	big := string(bigJSON(4096))
	tests := []struct {
		name    string
		headers map[string]string
		handler http.HandlerFunc
		want    string
	}{
		{
			name:    "a caller that does not accept gzip",
			headers: nil,
			handler: jsonBody(4096).ServeHTTP,
			want:    big,
		},
		{
			name:    "a caller that refuses gzip",
			headers: map[string]string{"Accept-Encoding": "br, gzip;q=0"},
			handler: jsonBody(4096).ServeHTTP,
			want:    big,
		},
		{
			name:    "a body under the threshold",
			headers: acceptGzip,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			},
			want: `{"detail":"Not found"}`,
		},
		{
			name:    "a body that is not JSON",
			headers: acceptGzip,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(big))
			},
			want: big,
		},
		{
			name:    "a body that is already encoded",
			headers: acceptGzip,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "br")
				_, _ = w.Write([]byte(big))
			},
			want: big,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// When the response passes through the middleware.
			rec := do(httpx.Compress(tt.handler), "GET", "/x", tt.headers)

			// Then the body is the handler's own, and nothing claims gzip.
			if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
				t.Errorf("Content-Encoding = %q", got)
			}
			if got := rec.Body.String(); got != tt.want {
				t.Errorf("body = %d bytes, want %d", len(got), len(tt.want))
			}
		})
	}
}

func TestCompressSendsNoBodyWhereThereIsNone(t *testing.T) {
	// Given a handler that answers 204.
	h := httpx.Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))

	// When a caller that accepts gzip asks.
	rec := do(h, "DELETE", "/x", acceptGzip)

	// Then there is no body and no encoding.
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("status = %d, body = %d bytes, encoding = %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Encoding"))
	}
}

func TestCompressDoesNotHoldBackAStream(t *testing.T) {
	// Given an event stream that flushes after each event.
	var sent *httptest.ResponseRecorder
	h := httpx.Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		// Then the first event has reached the client before the handler returns.
		if got := sent.Body.String(); got != "data: one\n\n" || !sent.Flushed {
			t.Errorf("after the flush the client has %q, flushed = %v", got, sent.Flushed)
		}
	}))

	// When a browser that accepts gzip opens it.
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	sent = httptest.NewRecorder()
	h.ServeHTTP(sent, req)
}

func TestCompressFlushesASmallJSONStreamAsWritten(t *testing.T) {
	// Given a handler that flushes JSON before the threshold.
	h := httpx.Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"n":1}`))
		_ = http.NewResponseController(w).Flush()
		_, _ = w.Write([]byte(`{"n":2}`))
	}))

	// When it is read.
	rec := do(h, "GET", "/x", acceptGzip)

	// Then both parts arrive uncompressed.
	if got := rec.Body.String(); got != `{"n":1}{"n":2}` || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("body = %q, encoding = %q", got, rec.Header().Get("Content-Encoding"))
	}
}

func TestCompressLeavesAnUpgradeAlone(t *testing.T) {
	// Given a WebSocket upgrade, which takes over the connection.
	var wrapped bool
	h := httpx.Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, wrapped = w.(*httptest.ResponseRecorder)
	}))

	// When it passes through.
	rec := do(h, "GET", "/sessions/s1/live", map[string]string{"Upgrade": "websocket", "Accept-Encoding": "gzip"})

	// Then the handler has the connection's own writer, and no header was added.
	if !wrapped || rec.Header().Get("Vary") != "" {
		t.Errorf("writer passed through = %v, Vary = %q", wrapped, rec.Header().Get("Vary"))
	}
}

func TestCompressLetsRecoveryAnswerAfterAPanic(t *testing.T) {
	// Given a handler that panics after writing part of a small JSON body.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"half":`))
		panic("boom")
	}), httpx.Recover(log), httpx.Compress)

	// When the request is served.
	rec := do(h, "GET", "/x", acceptGzip)

	// Then the caller gets the recovery's 500, not half a body with a 200.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "half") {
		t.Errorf("the held bytes were sent: %q", rec.Body.String())
	}
}

func TestCompressKeepsTheVaryValueCORSAdds(t *testing.T) {
	// Given the owner API's order: CORS outside compression.
	h := httpx.Chain(jsonBody(4096), httpx.CORS("https://mainloop.example", false), httpx.Compress)

	// When the frontend reads a large answer.
	rec := do(h, "GET", "/tasks", map[string]string{"Origin": "https://mainloop.example", "Accept-Encoding": "gzip"})

	// Then the answer varies on both headers, is compressed, and still carries the CORS grant.
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q", vary)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Access-Control-Allow-Origin") != "https://mainloop.example" {
		t.Errorf("encoding = %q, allow-origin = %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Access-Control-Allow-Origin"))
	}
}
