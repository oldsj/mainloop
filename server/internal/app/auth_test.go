package app_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
)

// The owner API in enforce mode: the whole chain, with responses checked against the contract.
func TestOwnerAPIRequiresAnIdentity(t *testing.T) {
	t.Parallel()
	iss := authtest.NewIssuer(t)
	cfg := newConfig(t, map[string]string{
		"MAINLOOP_AUTH_MODE":          "enforce",
		"MAINLOOP_AUTH_OIDC_ISSUER":   iss.URL(),
		"MAINLOOP_AUTH_OIDC_AUDIENCE": "mainloop",
		"MAINLOOP_AUTH_OWNER_SUBJECT": "owner-sub",
	})
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	a, err := auth.New(cfg, nil, iss.Client(), log)
	if err != nil {
		t.Fatal(err)
	}
	deps := apptest.Deps(t, cfg, log, nil)
	deps.DB = up()
	deps.Auth = a
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	h = apicheck.Wrap(t, h)
	owner := iss.Sign(t, authtest.Claims{"sub": "owner-sub", "aud": "mainloop"})

	t.Run("health stays open", func(t *testing.T) {
		if r := send(h, "GET", "/health", "", nil); r.status != http.StatusOK {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("no credential is 401 before validation", func(t *testing.T) {
		// An invalid body would be 422 for an owner; an anonymous caller learns nothing from it.
		r := send(h, "POST", "/schedules", `{"bad":true}`, map[string]string{"Origin": "https://mainloop.test"})
		if r.status != http.StatusUnauthorized || detail(t, r) != "Authentication required" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		if r.header.Get("WWW-Authenticate") == "" || r.header.Get("Access-Control-Allow-Origin") != "https://mainloop.test" {
			t.Fatalf("headers = %v", r.header)
		}
	})
	t.Run("the owner's cookie reaches the handler", func(t *testing.T) {
		r := send(h, "GET", "/schedules", "", map[string]string{"Cookie": "mainloop-id=" + owner})
		if r.status != http.StatusServiceUnavailable { // the schedules handler: no kagent here
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("an owner write needs the client header", func(t *testing.T) {
		body := `{"request_id":"r1","agent":{"namespace":"kagent","name":"a1"},"config":{"schedule":"0 * * * *","prompt":"go"}}`
		r := send(h, "POST", "/schedules", body, map[string]string{"Authorization": "Bearer " + owner, "Origin": "https://mainloop.test"})
		if r.status != http.StatusForbidden {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		r = send(h, "POST", "/schedules", body, map[string]string{"Authorization": "Bearer " + owner, "Origin": "https://mainloop.test", auth.ClientHeader: "web"})
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("loopback is not identity", func(t *testing.T) {
		r := send(h, "GET", "/schedules", "", map[string]string{"Host": "api.test", "X-Forwarded-For": "127.0.0.1"})
		if r.status != http.StatusUnauthorized {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
}
