package auth_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
	"github.com/oldsj/mainloop/server/internal/config"
)

const (
	frontend = "https://mainloop.test"
	ownerSub = "owner-sub"
	saAud    = "mainloop-api"
)

// reviewer is a fake TokenReview: tokens it knows map to a username.
type reviewer struct {
	users map[string]string
	calls atomic.Int32
	err   error
}

func (r *reviewer) Review(_ context.Context, token string, audiences []string) (auth.Review, error) {
	r.calls.Add(1)
	if r.err != nil {
		return auth.Review{}, r.err
	}
	u, ok := r.users[token]
	return auth.Review{Authenticated: ok, Username: u, Audiences: audiences}, nil
}

type fixture struct {
	iss    *authtest.Issuer
	rev    *reviewer
	logs   *bytes.Buffer
	h      http.Handler
	seen   *auth.Principal
	called bool
}

func newFixture(t *testing.T, mode auth.Mode) *fixture {
	t.Helper()
	f := &fixture{iss: authtest.NewIssuer(t), rev: &reviewer{users: map[string]string{}}, logs: &bytes.Buffer{}}
	a := &auth.Authenticator{
		Mode: mode, OIDC: &auth.OIDC{Issuer: f.iss.URL(), Audience: "mainloop", Client: f.iss.Client()},
		OwnerSubject: ownerSub, Cookie: "mainloop-id",
		ServiceAccounts: &auth.ServiceAccounts{Audience: saAud, Reviewer: f.rev},
		Scopes: map[string]auth.Scope{
			"mainloop/mainloop-canary":   auth.ScopeCanary,
			"mainloop/mainloop-backend":  auth.ScopeRead,
			"mainloop/mainloop-operator": auth.ScopeOwner,
		},
		FrontendOrigin: frontend, Log: slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	mw, err := a.Middleware(spec)
	if err != nil {
		t.Fatal(err)
	}
	f.h = mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.called = true
		if p, ok := auth.FromContext(r.Context()); ok {
			f.seen = &p
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return f
}

func (f *fixture) sa(t *testing.T, name string) string {
	t.Helper()
	ns, n, _ := strings.Cut(name, "/")
	tok := authtest.ServiceAccountToken(t, ns, n, saAud, time.Now().Add(10*time.Minute))
	f.rev.users[tok] = "system:serviceaccount:" + ns + ":" + n
	return tok
}

func (f *fixture) owner(t *testing.T, extra authtest.Claims) string {
	t.Helper()
	c := authtest.Claims{"sub": ownerSub, "aud": "mainloop"}
	for k, v := range extra {
		c[k] = v
	}
	return f.iss.Sign(t, c)
}

type req struct {
	method, path string
	bearer       string
	cookie       string
	headers      map[string]string
}

func (f *fixture) do(r req) *httptest.ResponseRecorder {
	f.called, f.seen = false, nil
	hr := httptest.NewRequestWithContext(context.Background(), r.method, "https://mainloop.test"+r.path, strings.NewReader("{}"))
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: "mainloop-id", Value: r.cookie})
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, hr)
	return w
}

var browserWrite = map[string]string{auth.ClientHeader: "web", "Origin": frontend}

func TestEnforce(t *testing.T) {
	f := newFixture(t, auth.ModeEnforce)
	owner := f.owner(t, nil)
	stranger := f.owner(t, authtest.Claims{"sub": "someone-else"})
	canary := f.sa(t, "mainloop/mainloop-canary")
	backend := f.sa(t, "mainloop/mainloop-backend")
	operator := f.sa(t, "mainloop/mainloop-operator")
	unlisted := f.sa(t, "mainloop/ops-tools")
	wrongAud := authtest.ServiceAccountToken(t, "mainloop", "mainloop-canary", "kagent-invocation", time.Now().Add(time.Hour))

	cases := []struct {
		name string
		req  req
		want int // 0: passed to the next handler
		kind string
	}{
		{"health is public", req{method: "GET", path: "/health"}, 0, ""},
		{"unknown path passes to the validator", req{method: "GET", path: "/nope"}, 0, ""},
		{"no credential", req{method: "GET", path: "/sessions"}, 401, ""},
		{"no credential on the live socket", req{method: "GET", path: "/sessions/s1/live"}, 401, ""},
		{"no credential on the event stream", req{method: "GET", path: "/events"}, 401, ""},
		{"malformed header", req{method: "GET", path: "/sessions", headers: map[string]string{"Authorization": "Basic abc"}}, 401, ""},
		{"garbage bearer", req{method: "GET", path: "/sessions", bearer: "abc"}, 401, ""},
		{"owner cookie reads", req{method: "GET", path: "/sessions", cookie: owner}, 0, auth.KindOwner},
		{"owner bearer reads", req{method: "GET", path: "/attention", bearer: owner}, 0, auth.KindOwner},
		{"owner writes from the frontend", req{method: "POST", path: "/sessions", cookie: owner, headers: browserWrite}, 0, auth.KindOwner},
		{"owner write without the client header", req{method: "POST", path: "/sessions", cookie: owner, headers: map[string]string{"Origin": frontend}}, 403, ""},
		{"owner write from a preview origin", req{method: "POST", path: "/sessions", cookie: owner, headers: map[string]string{auth.ClientHeader: "web", "Origin": "https://3000--w1--preview.preview.test"}}, 403, ""},
		{"owner bearer write is held to the same rule", req{method: "POST", path: "/sessions", bearer: owner}, 403, ""},
		{"native client write: header, no Origin", req{method: "POST", path: "/sessions", bearer: owner, headers: map[string]string{auth.ClientHeader: "ios"}}, 0, auth.KindOwner},
		{"another Pocket ID user", req{method: "GET", path: "/sessions", cookie: stranger}, 403, ""},
		{"canary on its routes", req{method: "POST", path: "/sessions/s1/message", bearer: canary}, 0, auth.KindServiceAccount},
		{"canary lists sessions", req{method: "GET", path: "/sessions", bearer: canary}, 0, auth.KindServiceAccount},
		{"canary off its routes", req{method: "GET", path: "/projects", bearer: canary}, 403, ""},
		{"read scope reads", req{method: "GET", path: "/projects", bearer: backend}, 0, auth.KindServiceAccount},
		{"read scope cannot write", req{method: "POST", path: "/sessions", bearer: backend}, 403, ""},
		{"read scope cannot open the live socket", req{method: "GET", path: "/sessions/s1/live", bearer: backend}, 403, ""},
		{"operator is the owner", req{method: "POST", path: "/sessions", bearer: operator}, 0, auth.KindServiceAccount},
		{"unlisted ServiceAccount", req{method: "GET", path: "/sessions", bearer: unlisted}, 403, ""},
		{"ServiceAccount token for another audience", req{method: "GET", path: "/sessions", bearer: wrongAud}, 401, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := f.do(c.req)
			if c.want == 0 {
				if !f.called {
					t.Fatalf("refused %d: %s; logs:\n%s", w.Code, w.Body, f.logs)
				}
				if c.kind == "" && f.seen != nil {
					t.Fatalf("principal %+v on an unsecured request", f.seen)
				}
				if c.kind != "" && (f.seen == nil || f.seen.Kind != c.kind) {
					t.Fatalf("principal = %+v, want kind %s", f.seen, c.kind)
				}
				return
			}
			if f.called || w.Code != c.want {
				t.Fatalf("status = %d (passed=%v), want %d", w.Code, f.called, c.want)
			}
			if c.want == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 without a challenge")
			}
			if strings.Contains(w.Body.String(), "subject") || strings.Contains(w.Body.String(), "token") {
				t.Fatalf("refusal leaks its reason: %s", w.Body)
			}
		})
	}
	if f.rev.calls.Load() == 0 {
		t.Fatal("no TokenReview was made")
	}
}

func TestObserveRefusesNothingAndLogsOnce(t *testing.T) {
	f := newFixture(t, auth.ModeObserve)
	for range 3 {
		if f.do(req{method: "GET", path: "/sessions"}); !f.called {
			t.Fatal("observe refused a request")
		}
	}
	if n := strings.Count(f.logs.String(), "auth: would_refuse"); n != 1 {
		t.Fatalf("would_refuse lines = %d, want 1:\n%s", n, f.logs)
	}
	f.do(req{method: "GET", path: "/sessions", cookie: f.owner(t, nil)})
	if f.seen == nil || f.seen.Kind != auth.KindOwner {
		t.Fatalf("observe did not attach the principal: %+v", f.seen)
	}
	if !strings.Contains(f.logs.String(), "auth: principal") {
		t.Fatalf("no principal line:\n%s", f.logs)
	}
}

func TestServiceAccountReviewsAreCached(t *testing.T) {
	rev := &reviewer{users: map[string]string{"good": "system:serviceaccount:mainloop:mainloop-canary", "user": "alice"}}
	now := time.Now()
	s := &auth.ServiceAccounts{Audience: saAud, Reviewer: rev, Now: func() time.Time { return now }}
	ctx := context.Background()
	exp := now.Add(10 * time.Minute)

	for range 3 {
		name, err := s.Verify(ctx, "good", exp)
		if err != nil || name != "mainloop/mainloop-canary" {
			t.Fatalf("Verify = %q, %v", name, err)
		}
	}
	if got := rev.calls.Load(); got != 1 {
		t.Fatalf("reviews = %d, want 1 (cached)", got)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Verify(ctx, "good", exp); err != nil {
		t.Fatal(err)
	}
	if got := rev.calls.Load(); got != 2 {
		t.Fatalf("reviews = %d, want 2 after the cache expired", got)
	}

	for _, tok := range []string{"unknown", "user"} {
		for range 2 {
			if _, err := s.Verify(ctx, tok, exp); err == nil {
				t.Fatalf("%s accepted", tok)
			}
		}
	}
	if got := rev.calls.Load(); got != 4 {
		t.Fatalf("reviews = %d, want 4 (refusals cached too)", got)
	}

	rev.err = errors.New("apiserver down")
	if _, err := s.Verify(ctx, "fresh", exp); err == nil {
		t.Fatal("accepted without a review")
	}
}

func TestNewModeRules(t *testing.T) {
	base := map[string]string{"AGENT_TOKEN_KEY": "k"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	oidc := []string{"MAINLOOP_AUTH_OIDC_ISSUER", "https://id.test", "MAINLOOP_AUTH_OIDC_AUDIENCE", "mainloop", "MAINLOOP_AUTH_OWNER_SUBJECT", "s"}
	cases := []struct {
		name string
		vars map[string]string
		want auth.Mode // "" means New fails
	}{
		{"unset outside dev fails", with(), ""},
		{"unset in dev is off", with("DEV_MODE", "true"), auth.ModeOff},
		{"off outside dev fails", with("MAINLOOP_AUTH_MODE", "off"), ""},
		{"observe without an issuer", with("MAINLOOP_AUTH_MODE", "observe"), auth.ModeObserve},
		{"enforce without an issuer fails", with("MAINLOOP_AUTH_MODE", "enforce"), ""},
		{"enforce with an issuer", with(append([]string{"MAINLOOP_AUTH_MODE", "enforce"}, oidc...)...), auth.ModeEnforce},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := config.Parse(c.vars)
			if err != nil {
				t.Fatal(err)
			}
			a, err := auth.New(cfg, nil, nil, slog.Default())
			if c.want == "" {
				if err == nil {
					t.Fatalf("New succeeded with mode %s", a.Mode)
				}
				return
			}
			if err != nil || a.Mode != c.want {
				t.Fatalf("New = %v, %v; want %s", a, err, c.want)
			}
		})
	}
}

func TestConfigRefusesBadAuthSettings(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"mode":         {"MAINLOOP_AUTH_MODE": "strict"},
		"http issuer":  {"MAINLOOP_AUTH_OIDC_ISSUER": "http://id.test", "MAINLOOP_AUTH_OIDC_AUDIENCE": "a", "MAINLOOP_AUTH_OWNER_SUBJECT": "s"},
		"issuer alone": {"MAINLOOP_AUTH_OIDC_ISSUER": "https://id.test", "MAINLOOP_AUTH_OWNER_SUBJECT": "s"},
		"no subject":   {"MAINLOOP_AUTH_OIDC_ISSUER": "https://id.test", "MAINLOOP_AUTH_OIDC_AUDIENCE": "a"},
		"bad scope":    {"MAINLOOP_AUTH_SERVICE_ACCOUNTS": "mainloop/canary=admin"},
		"bad entry":    {"MAINLOOP_AUTH_SERVICE_ACCOUNTS": "canary=owner"},
	} {
		t.Run(name, func(t *testing.T) {
			vars["AGENT_TOKEN_KEY"] = "k"
			if _, err := config.Parse(vars); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k",
		"MAINLOOP_AUTH_SERVICE_ACCOUNTS": " mainloop/mainloop-canary=canary, mainloop/mainloop-backend=read "})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.ServiceAccounts["mainloop/mainloop-canary"] != "canary" || cfg.Auth.ServiceAccounts["mainloop/mainloop-backend"] != "read" {
		t.Fatalf("ServiceAccounts = %v", cfg.Auth.ServiceAccounts)
	}
}
