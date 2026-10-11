package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
)

type pinger func(context.Context) error

func (p pinger) Ping(ctx context.Context) error { return p(ctx) }

func newHandler(t *testing.T, vars map[string]string, ping app.Pinger) http.Handler {
	t.Helper()
	all := map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"}
	for k, v := range vars {
		all[k] = v
	}
	cfg, err := config.Parse(all)
	if err != nil {
		t.Fatal(err)
	}
	deps := app.Deps{Config: cfg, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), DB: ping}
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func up() app.Pinger { return pinger(func(context.Context) error { return nil }) }

type result struct {
	status int
	body   string
	header http.Header
}

func send(h http.Handler, method, path, body string, headers map[string]string) result {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	req.Host = "api.test"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return result{rec.Code, rec.Body.String(), rec.Header()}
}

func detail(t *testing.T, r result) any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	return body["detail"]
}

func TestUnimplementedOperationReturns501WithErrorShape(t *testing.T) {
	h := newHandler(t, nil, up())
	for _, path := range []string{"/schedules", "/schedules/s1"} {
		r := send(h, "GET", path, "", nil)
		if r.status != http.StatusNotImplemented {
			t.Fatalf("%s: status = %d, body %s", path, r.status, r.body)
		}
		if got := detail(t, r); got != "Not implemented" {
			t.Errorf("%s: detail = %v", path, got)
		}
		if r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Request-ID") == "" {
			t.Errorf("%s: headers = %v", path, r.header)
		}
	}
}

func TestHealth(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		r := send(newHandler(t, nil, up()), "GET", "/health", "", nil)
		if r.status != 200 || !strings.Contains(r.body, `"status":"healthy"`) {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("database down", func(t *testing.T) {
		h := newHandler(t, nil, pinger(func(context.Context) error { return errors.New("dial tcp: password=hunter2") }))
		r := send(h, "GET", "/health", "", nil)
		if r.status != http.StatusServiceUnavailable || detail(t, r) != "Database unavailable" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		if strings.Contains(r.body, "hunter2") {
			t.Error("ping error leaked")
		}
	})
	t.Run("pod ip host is exempt", func(t *testing.T) {
		r := send(newHandler(t, nil, up()), "GET", "/health", "", map[string]string{"Host": "10.244.0.7:8000"})
		if r.status != 200 {
			t.Fatalf("status = %d", r.status)
		}
	})
	t.Run("real database", func(t *testing.T) {
		url := config.TestDatabaseURL()
		if url == "" {
			t.Skip("MAINLOOP_TEST_DATABASE_URL is not set")
		}
		pool, err := db.NewPoolFromURL(t.Context(), url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		r := send(newHandler(t, nil, pool), "GET", "/health", "", nil)
		if r.status != 200 || !strings.Contains(r.body, `"database":"ok"`) {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		assertPool(t, pool)
	})
	t.Run("unreachable database", func(t *testing.T) {
		pool, err := db.NewPoolFromURL(t.Context(), "postgres://u:p@127.0.0.1:1/x?connect_timeout=1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		r := send(newHandler(t, nil, pool), "GET", "/health", "", nil)
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
}

func assertPool(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var path string
	if err := pool.QueryRow(t.Context(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != db.Schema {
		t.Errorf("search_path = %q, want %q", path, db.Schema)
	}
	if got := pool.Config().MaxConns; got != 10 {
		t.Errorf("max conns = %d", got)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHandler(t, nil, up())
	tests := []struct {
		name         string
		method, path string
		body         string
		status       int
		detail       string
	}{
		{"unknown path", "GET", "/nope", "", 404, "Not Found"},
		{"wrong method", "DELETE", "/health", "", 405, "Method Not Allowed"},
		{"missing body", "POST", "/chat", "", 422, ""},
		{"wrong body type", "POST", "/chat", `{"message": 5}`, 422, ""},
		{"valid body reaches the 501 fallback", "POST", "/schedules", `{"request_id": "r1", "agent": {"namespace": "kagent", "name": "a1"}, "config": {"schedule": "0 * * * *", "prompt": "go"}}`, 501, "Not implemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := send(h, tt.method, tt.path, tt.body, nil)
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.detail != "" && detail(t, r) != tt.detail {
				t.Errorf("detail = %v", detail(t, r))
			}
			if tt.status == 422 {
				list, ok := detail(t, r).([]any)
				if !ok || len(list) == 0 {
					t.Fatalf("422 detail is not a list: %s", r.body)
				}
				entry := list[0].(map[string]any)
				for _, k := range []string{"loc", "msg", "type"} {
					if _, ok := entry[k]; !ok {
						t.Errorf("422 entry lacks %q: %v", k, entry)
					}
				}
			}
		})
	}
}

func TestGuardsApplyToTheAPI(t *testing.T) {
	h := newHandler(t, nil, up())
	t.Run("foreign host", func(t *testing.T) {
		r := send(h, "GET", "/schedules", "", map[string]string{"Host": "evil.example"})
		if r.status != 404 || detail(t, r) != "Not Found" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("foreign origin write", func(t *testing.T) {
		r := send(h, "POST", "/chat", `{"message":"hi"}`, map[string]string{"Origin": "https://evil.example"})
		if r.status != 403 || detail(t, r) != "Cross-origin request refused." {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("frontend origin write passes", func(t *testing.T) {
		r := send(h, "POST", "/schedules", `{"request_id":"r1","agent":{"namespace":"kagent","name":"a1"},"config":{"schedule":"0 * * * *","prompt":"go"}}`, map[string]string{"Origin": "https://mainloop.test"})
		if r.status != 501 {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("cors preflight", func(t *testing.T) {
		r := send(h, "OPTIONS", "/chat", "", map[string]string{
			"Origin": "https://mainloop.test", "Access-Control-Request-Method": "POST",
		})
		if got := r.header.Get("Access-Control-Allow-Origin"); got != "https://mainloop.test" {
			t.Fatalf("allow-origin = %q (status %d)", got, r.status)
		}
	})
	t.Run("dev allows loopback host", func(t *testing.T) {
		dev := newHandler(t, map[string]string{"DEV_MODE": "true"}, up())
		if r := send(dev, "GET", "/schedules", "", map[string]string{"Host": "localhost:8000"}); r.status != 501 {
			t.Fatalf("got %d", r.status)
		}
		if r := send(h, "GET", "/schedules", "", map[string]string{"Host": "localhost:8000"}); r.status != 404 {
			t.Fatalf("prod loopback got %d", r.status)
		}
	})
}

func TestStubHandler(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if err != nil {
		t.Fatal(err)
	}
	h := app.StubHandler(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), cfg.AllowedMCPHosts())
	host := map[string]string{"Host": "mainloop-mcp.mainloop.svc.cluster.local:8002"}
	if r := send(h, "GET", "/health", "", host); r.status != 200 {
		t.Errorf("health = %d", r.status)
	}
	if r := send(h, "POST", "/mcp/anything", "", host); r.status != 501 || detail(t, r) != "Not implemented" {
		t.Errorf("fallback = %d %s", r.status, r.body)
	}
	if r := send(h, "GET", "/mcp/anything", "", map[string]string{"Host": "evil.example"}); r.status != 404 {
		t.Errorf("foreign host = %d", r.status)
	}
}

func TestRequestBodyCap(t *testing.T) {
	h := newHandler(t, nil, up())
	huge := `{"message": "` + strings.Repeat("a", 5<<20) + `"}`
	// Declared length: refused before the validator reads anything.
	r := send(h, "POST", "/chat", huge, nil)
	if r.status != http.StatusRequestEntityTooLarge || detail(t, r) != "Request body too large" {
		t.Fatalf("declared: got %d %.100s", r.status, r.body)
	}
	// Undeclared length: the validator hits the cap while reading.
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/chat", io.MultiReader(strings.NewReader(huge)))
	req.Host = "api.test"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: got %d %.100s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["detail"] == "" {
		t.Errorf("chunked body is not {detail}: %.100s", rec.Body.String())
	}
}

func TestHealthLogsPingFailure(t *testing.T) {
	var logs bytes.Buffer
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	deps := app.Deps{Config: cfg, Log: slog.New(slog.NewJSONHandler(&logs, nil)),
		DB: pinger(func(context.Context) error { return errors.New("connection refused") })}
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	if r := send(h, "GET", "/health", "", nil); r.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", r.status)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("log = %s", logs.String())
	}
}
