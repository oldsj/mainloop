package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

func do(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	req.Host = "api.test"
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func detail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if len(body) != 1 {
		t.Errorf("body has extra keys: %v", body)
	}
	return body["detail"]
}

// Cases mined from backend/tests/runtime/test_origin_guard.py.
func TestOriginGuard(t *testing.T) {
	const frontend = "https://mainloop.example.ts.net"
	writes := []string{"/sessions/s1/cancel", "/sessions/s1/stop-turn", "/sessions/s1/archive", "/workspaces/s1/suspend"}
	tests := []struct {
		name    string
		dev     bool
		method  string
		origin  *string
		host    string
		refused bool
	}{
		{"preview origin", false, "POST", p("https://5173--workspace--preview.mainloop.example"), "api.test", true},
		{"evil origin", false, "POST", p("https://evil.example"), "api.test", true},
		{"localhost without port", false, "POST", p("http://localhost"), "api.test", true},
		{"null origin", false, "POST", p("null"), "api.test", true},
		{"empty origin", false, "POST", p(""), "api.test", true},
		{"frontend origin", false, "POST", p(frontend), "api.test", false},
		{"localhost port in dev", true, "POST", p("http://localhost:5173"), "api.test", false},
		{"localhost port in prod", false, "POST", p("http://localhost:5173"), "api.test", true},
		{"localhost lookalike in dev", true, "POST", p("http://localhost:5173.evil.example"), "api.test", true},
		{"localhost https in dev", true, "POST", p("https://localhost:5173"), "api.test", true},
		{"own origin", false, "POST", p("http://api.test"), "api.test", false},
		{"own origin other port", false, "POST", p("http://api.test:9999"), "api.test", true},
		{"no origin header", false, "POST", nil, "api.test", false},
		{"get with foreign origin", false, "GET", p("https://evil.example"), "api.test", false},
		{"preflight with foreign origin", false, "OPTIONS", p("https://evil.example"), "api.test", false},
		{"delete is a write", false, "DELETE", p("https://evil.example"), "api.test", true},
		{"put is a write", false, "PUT", p("https://evil.example"), "api.test", true},
		{"patch is a write", false, "PATCH", p("https://evil.example"), "api.test", true},
	}
	for _, tt := range tests {
		for _, path := range writes {
			t.Run(tt.name+path, func(t *testing.T) {
				h := httpx.OriginGuard(frontend, tt.dev)(ok)
				headers := map[string]string{"Host": tt.host}
				if tt.origin != nil {
					headers["Origin"] = *tt.origin
				}
				rec := do(h, tt.method, path, headers)
				if tt.refused {
					if rec.Code != http.StatusForbidden || detail(t, rec) != "Cross-origin request refused." {
						t.Fatalf("got %d %q, want refusal", rec.Code, rec.Body.String())
					}
				} else if rec.Code != http.StatusOK {
					t.Fatalf("got %d, want pass", rec.Code)
				}
			})
		}
	}
}

func p(s string) *string { return &s }

// Cases mined from backend/tests/runtime/test_host_guard.py.
func TestHostGuard(t *testing.T) {
	const frontend = "mainloop.example.ts.net"
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": frontend, "MAINLOOP_API_HOSTS": "mainloop-backend"})
	if err != nil {
		t.Fatal(err)
	}
	allowed := cfg.AllowedAPIHosts()
	tests := []struct {
		host string
		dev  bool
		path string
		want int
	}{
		{frontend, false, "/", 200},
		{frontend + ":8443", false, "/", 200},
		{strings.ToUpper(frontend), false, "/", 200},
		{"mainloop-backend:8000", false, "/", 200},
		{"localhost:8000", true, "/", 200},
		{"127.0.0.1:8000", true, "/", 200},
		{"[::1]:8000", true, "/", 200},
		{"localhost:8000", false, "/", 404},
		{"evil.example", false, "/", 404},
		{"evil.example:8443", false, "/", 404},
		{frontend + ".evil.example", false, "/", 404},
		{"evil." + frontend, false, "/", 404},
		{"previews.example.test:8001", false, "/", 404},
		{"", false, "/", 404},
		{"[bad", false, "/", 404},
		{"user@" + frontend, false, "/", 404},
		{frontend + "/x", false, "/", 404},
		{"10.244.0.7:8000", false, "/health", 200},
		{"10.244.0.7:8000", false, "/sessions", 404},
		{"10.244.0.7:8000", false, "/health/extra", 404},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q dev=%v %s", tt.host, tt.dev, tt.path), func(t *testing.T) {
			h := httpx.HostGuard(allowed, tt.dev, "/health")(ok)
			rec := do(h, "GET", tt.path, map[string]string{"Host": tt.host})
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == 404 && detail(t, rec) != "Not Found" {
				t.Errorf("body = %q", rec.Body.String())
			}
		})
	}
}

func TestCORS(t *testing.T) {
	const frontend = "https://mainloop.example.ts.net"
	preflight := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		return do(httpx.Chain(h), "OPTIONS", "/sessions/s1/cancel", map[string]string{
			"Origin": origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
		})
	}
	t.Run("frontend", func(t *testing.T) {
		rec := preflight(httpx.CORS(frontend, false)(ok), frontend)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != frontend {
			t.Errorf("allow-origin = %q", got)
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Error("credentials not allowed")
		}
	})
	t.Run("foreign origin gets no CORS headers", func(t *testing.T) {
		rec := preflight(httpx.CORS(frontend, false)(ok), "https://evil.example")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("allow-origin = %q", got)
		}
	})
	t.Run("localhost only in dev", func(t *testing.T) {
		if got := preflight(httpx.CORS(frontend, true)(ok), "http://localhost:5173").Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
			t.Errorf("dev allow-origin = %q", got)
		}
		if got := preflight(httpx.CORS(frontend, false)(ok), "http://localhost:5173").Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("prod allow-origin = %q", got)
		}
	})
}

func TestRequestID(t *testing.T) {
	var seen string
	h := httpx.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	t.Run("generated", func(t *testing.T) {
		rec := do(h, "GET", "/", nil)
		if id := rec.Header().Get(httpx.RequestIDHeader); len(id) != 16 || id != seen {
			t.Errorf("id = %q, ctx = %q", id, seen)
		}
	})
	t.Run("reused", func(t *testing.T) {
		rec := do(h, "GET", "/", map[string]string{httpx.RequestIDHeader: "abc-123.x_y"})
		if rec.Header().Get(httpx.RequestIDHeader) != "abc-123.x_y" || seen != "abc-123.x_y" {
			t.Errorf("id = %q, ctx = %q", rec.Header().Get(httpx.RequestIDHeader), seen)
		}
	})
	t.Run("malformed replaced", func(t *testing.T) {
		for _, bad := range []string{strings.Repeat("a", 65), "has space", "semi;colon", "new\tline"} {
			rec := do(h, "GET", "/", map[string]string{httpx.RequestIDHeader: bad})
			if got := rec.Header().Get(httpx.RequestIDHeader); got == bad || len(got) != 16 {
				t.Errorf("%q kept as %q", bad, got)
			}
		}
	})
}

func TestRecover(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := httpx.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("db password hunter2")
	}))
	rec := do(h, "GET", "/", nil)
	if rec.Code != http.StatusInternalServerError || detail(t, rec) != "Internal server error" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("panic value leaked to the client")
	}
	if !strings.Contains(logs.String(), "hunter2") || !strings.Contains(logs.String(), "stack") {
		t.Errorf("panic not logged: %s", logs.String())
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := httpx.Recover(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if recover() != http.ErrAbortHandler { //nolint:errorlint // sentinel identity
			t.Error("ErrAbortHandler was swallowed")
		}
	}()
	do(h, "GET", "/", nil)
}

func TestWriteErr(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{"typed", httpx.NewError(http.StatusConflict, "Session is archived"), 409, "Session is archived"},
		{"wrapped", fmt.Errorf("outer: %w", httpx.NewError(http.StatusNotFound, "Session not found")), 404, "Session not found"},
		{"not implemented", httpx.ErrNotImplemented, 501, "Not implemented"},
		{"unknown", errors.New("pq: password authentication failed for hunter2"), 500, "Internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpx.WriteErr(rec, tt.err)
			if rec.Code != tt.status || detail(t, rec) != tt.detail {
				t.Errorf("got %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	do(httpx.Chain(ok, mw("a"), mw("b")), "GET", "/", nil)
	if strings.Join(order, ",") != "a,b" {
		t.Errorf("order = %v", order)
	}
}

func TestAccessLogOmitsQuery(t *testing.T) {
	var logs bytes.Buffer
	h := httpx.AccessLog(slog.New(slog.NewJSONHandler(&logs, nil)))(ok)
	do(h, "GET", "/sessions?token=secret", nil)
	if strings.Contains(logs.String(), "secret") || !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("log = %s", logs.String())
	}
}
