package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/preview"
)

type noWake struct{}

func (noWake) WakeForPreview(context.Context, string) error { return nil }
func (noWake) Touch(context.Context, string) error          { return nil }

func previewHandler(t *testing.T, withPreview bool) http.Handler {
	t.Helper()
	cfg, err := config.Parse(map[string]string{
		"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test",
		"SUBSTRATE_PREVIEW_BASE_URL": "http://preview.test:8001",
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	deps := app.Deps{Config: cfg, Log: log, DB: up()}
	if withPreview {
		// No pool: these requests are all decided before the database is read.
		deps.Preview, err = preview.New(cfg, nil, noWake{}, log)
		if err != nil {
			t.Fatal(err)
		}
	}
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPreviewHostsAreRoutedAheadOfTheAPIHostGuard(t *testing.T) {
	const host = "3000--ws1--preview.preview.test:8001"
	h := previewHandler(t, true)

	// A cross-origin POST to a preview host is answered by the proxy (403 with its own body), not
	// by the API's Host guard, and carries the request id the outer middleware sets.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Origin", "https://evil.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Body.String() != "Cross-origin preview request refused" {
		t.Fatalf("preview host: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("preview response has no request id")
	}

	// The API host keeps working.
	if got := send(h, http.MethodGet, "/health", "", nil); got.status != http.StatusOK {
		t.Fatalf("API /health: %d", got.status)
	}
}

func TestPreviewHostIsRefusedWithoutAProxy(t *testing.T) {
	const host = "3000--ws1--preview.preview.test:8001"
	h := previewHandler(t, false)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+"/projects", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("preview host reached the API: %d", rec.Code)
	}
}
