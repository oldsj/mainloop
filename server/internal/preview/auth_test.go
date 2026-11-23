package preview

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/auth"
)

// nothingHappened fails the test if the request looked the workspace up far enough to wake or
// touch it, or opened a tunnel.
func (e *env) nothingHappened(what string) {
	e.t.Helper()
	if w, tc := e.waker.counts(); w != 0 || tc != 0 || len(e.router.seen()) != 0 {
		e.t.Fatalf("%s: wakes=%d touches=%d tunnels=%d, want none", what, w, tc, len(e.router.seen()))
	}
}

func TestAnonymousNavigationGoesToTheAppToSignIn(t *testing.T) {
	e := newEnv(t)
	for _, headers := range []map[string]string{
		{anon: "1", "Sec-Fetch-Mode": "navigate", "Accept": "*/*"},
		{anon: "1", "Accept": "text/html,application/xhtml+xml"}, // no fetch metadata
	} {
		rec := e.do("GET", wsHost, "/a/b?x=1", nil, headers)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%v: got %d, want 303", headers, rec.Code)
		}
		want := "https://mainloop.test/previews/ws1/3000?path=%2Fa%2Fb%3Fx%3D1"
		if loc := rec.Header().Get("Location"); loc != want {
			t.Fatalf("Location = %q, want %q", loc, want)
		}
	}
	// A workspace that does not exist is answered the same way: nothing is looked up.
	if rec := e.do("GET", "3000--nope--preview."+baseHost, "/", nil, map[string]string{anon: "1", "Sec-Fetch-Mode": "navigate"}); rec.Code != http.StatusSeeOther {
		t.Fatalf("unknown workspace: got %d, want 303", rec.Code)
	}
	e.nothingHappened("anonymous navigation")
}

func TestAnonymousNonNavigationIs401(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method  string
		headers map[string]string
	}{
		{"GET", map[string]string{"Sec-Fetch-Mode": "cors", "Accept": "text/html"}},
		{"GET", map[string]string{}}, // curl
		{"POST", map[string]string{"Origin": "http://" + wsHost}},
		{"GET", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Origin": "http://" + wsHost}},
		{"DELETE", map[string]string{"Sec-Fetch-Mode": "navigate"}},
	}
	for _, c := range cases {
		c.headers[anon] = "1"
		rec := e.do(c.method, wsHost, "/", strings.NewReader(""), c.headers)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %v: got %d, want 401", c.method, c.headers, rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Fatalf("%s %v: redirected", c.method, c.headers)
		}
	}
	e.nothingHappened("anonymous requests")
}

func TestPreviewCookieIsScopedToOneHostAndExpires(t *testing.T) {
	e := newEnv(t)
	e.seed("ws2", owner, "kagent-sess-2", `[{"name":"web","number":3000}]`)
	h := Host{Port: 3000, WorkspaceID: "ws1"}
	exp := time.Now().Add(time.Hour)
	other := e.proxy.mint(kindSession, Host{Port: 8080, WorkspaceID: "ws1"}, exp)
	sibling := e.proxy.mint(kindSession, Host{Port: 3000, WorkspaceID: "ws2"}, exp)
	expired := e.proxy.mint(kindSession, h, time.Now().Add(-time.Second))
	grant := e.proxy.mint(kindGrant, h, exp)
	good := e.proxy.mint(kindSession, h, exp)
	tampered := good[:len(good)-2] + "AA"
	if tampered == good {
		tampered = good[:len(good)-2] + "BB"
	}
	enc, _, _ := strings.Cut(good, ".")
	forged := enc + "." + strings.Repeat("A", 43)
	for name, v := range map[string]string{
		"another port": other, "another workspace": sibling, "expired": expired, "a grant": grant,
		"tampered": tampered, "forged": forged, "empty": "",
	} {
		rec := e.do("GET", wsHost, "/", nil, map[string]string{anon: "1", "Cookie": e.proxy.cookieName() + "=" + v})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", name, rec.Code)
		}
	}
	e.nothingHappened("refused cookies")
	// One valid cookie among others is enough.
	rec := e.do("GET", wsHost, "/", nil, map[string]string{anon: "1", "Cookie": e.proxy.cookieName() + "=" + other + "; " + e.proxy.cookieName() + "=" + good})
	if rec.Code != http.StatusOK || rec.Body.String() != "dev" {
		t.Fatalf("valid cookie: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestGrantExchangeSetsAHostOnlyCookie(t *testing.T) {
	e := newEnv(t)
	raw, exp, err := e.proxy.Grant(context.Background(), "ws1", 3000, "/a?b=1")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d <= 0 || d > GrantTTL {
		t.Fatalf("grant expires in %v", d)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Host != wsHost || u.Path != AuthPath || u.RawQuery != "" {
		t.Fatalf("grant URL %q: the sign-in must be on the preview host, in the fragment only", raw)
	}
	frag, err := url.ParseQuery(u.Fragment)
	if err != nil || frag.Get("next") != "/a?b=1" || frag.Get("t") == "" {
		t.Fatalf("fragment %q: %v", u.Fragment, err)
	}

	// The sign-in page itself: no workspace work, a nonce-pinned script, no referrer.
	page := e.do("GET", wsHost, AuthPath, nil, map[string]string{anon: "1"})
	csp := page.Header().Get("Content-Security-Policy")
	if page.Code != http.StatusOK || !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		page.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(page.Body.String(), "replaceState") {
		t.Fatalf("sign-in page: %d %v", page.Code, page.Header())
	}

	form := url.Values{"t": {frag.Get("t")}}.Encode()
	rec := e.do("POST", wsHost, AuthPath, strings.NewReader(form), map[string]string{
		anon: "1", "Origin": "http://" + wsHost, "Content-Type": "application/x-www-form-urlencoded",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("exchange: got %d %q", rec.Code, rec.Body.String())
	}
	e.nothingHappened("grant and exchange")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != "mainloop-preview" || c.Domain != "" || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != int(SessionTTL.Seconds()) {
		t.Fatalf("cookie attributes: %+v", c)
	}
	ok := e.do("GET", wsHost, "/a?b=1", nil, map[string]string{anon: "1", "Cookie": c.Name + "=" + c.Value})
	if ok.Code != http.StatusOK || ok.Body.String() != "dev" {
		t.Fatalf("with the cookie: got %d %q", ok.Code, ok.Body.String())
	}
	// The cookie is no good on another preview host.
	if rec := e.do("GET", "8080--ws1--preview."+baseHost, "/", nil, map[string]string{anon: "1", "Cookie": c.Name + "=" + c.Value}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on port 8080: got %d, want 401", rec.Code)
	}
	// The grant is no good on another host, nor after it expires.
	if rec := e.do("POST", "8080--ws1--preview."+baseHost, AuthPath, strings.NewReader(form), map[string]string{
		anon: "1", "Content-Type": "application/x-www-form-urlencoded",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("grant on another host: got %d", rec.Code)
	}
	e.proxy.now = func() time.Time { return time.Now().Add(GrantTTL + time.Second) }
	if rec := e.do("POST", wsHost, AuthPath, strings.NewReader(form), map[string]string{
		anon: "1", "Content-Type": "application/x-www-form-urlencoded",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired grant: got %d", rec.Code)
	}
}

func TestSignInPathsAreNeverForwarded(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/__mainloop/other", "/__mainloop/"} {
		if rec := e.do("GET", wsHost, path, nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", path, rec.Code)
		}
	}
	if rec := e.do("PUT", wsHost, AuthPath, strings.NewReader(""), nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT sign-in: got %d", rec.Code)
	}
	// A cross-origin exchange is refused by the Origin check before it is read.
	if rec := e.do("POST", wsHost, AuthPath, strings.NewReader("t=x"), map[string]string{"Origin": "http://mainloop.test"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin exchange: got %d, want 403", rec.Code)
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("a sign-in path reached the dev server")
	}
}

func TestGrantRefusals(t *testing.T) {
	e := newEnv(t)
	e.seed("ws-other", "someone-else", "kagent-sess-9", `[{"name":"web","number":3000}]`)
	e.seed("ws-creating", owner, "", `[{"name":"web","number":3000}]`)
	ctx := context.Background()
	for _, c := range []struct {
		ws   string
		port int
		path string
		want error
	}{
		{"nope", 3000, "/", ErrNotPreviewable},
		{"ws-other", 3000, "/", ErrNotPreviewable},
		{"ws-creating", 3000, "/", ErrNotPreviewable},
		{"Bad_ID", 3000, "/", ErrNotPreviewable},
		{"ws1", 5000, "/", ErrPortNotDeclared},
		{"ws1", 3000, "//evil.test/", ErrBadPath},
		{"ws1", 3000, "/\\evil.test", ErrBadPath},
		{"ws1", 3000, "https://evil.test/", ErrBadPath},
		{"ws1", 3000, "/a\nb", ErrBadPath},
		{"ws1", 3000, "/" + strings.Repeat("a", maxNextPath), ErrBadPath},
	} {
		if _, _, err := e.proxy.Grant(ctx, c.ws, c.port, c.path); !errors.Is(err, c.want) {
			t.Errorf("Grant(%s, %d, %q) = %v, want %v", c.ws, c.port, c.path, err, c.want)
		}
	}
	e.nothingHappened("refused grants")
}

func TestCookieNameIsHostPrefixedOverHTTPS(t *testing.T) {
	b, _ := parseBase("https://preview.example.test")
	if got := (&Proxy{base: b}).cookieName(); got != "__Host-mainloop-preview" {
		t.Fatalf("https cookie name = %q", got)
	}
}

func TestGrantAPIRequiresAnOwner(t *testing.T) {
	g := &GrantAPI{}
	req := api.GrantWorkspacePreviewRequestObject{WorkspaceId: "ws1", Port: 3000}
	resp, err := g.GrantWorkspacePreview(context.Background(), req)
	if _, ok := resp.(api.GrantWorkspacePreview401JSONResponse); !ok || err != nil {
		t.Fatalf("anonymous: %T %v, want 401", resp, err)
	}
	for _, p := range []auth.Principal{
		{Kind: auth.KindServiceAccount, Name: "mainloop/mainloop-backend", Scope: auth.ScopeRead},
		{Kind: auth.KindServiceAccount, Name: "mainloop/mainloop-canary", Scope: auth.ScopeCanary},
	} {
		resp, err := g.GrantWorkspacePreview(auth.WithPrincipal(context.Background(), p), req)
		if _, ok := resp.(api.GrantWorkspacePreview403JSONResponse); !ok || err != nil {
			t.Fatalf("%s: %T %v, want 403", p.Scope, resp, err)
		}
	}
	// The owner gets through to the proxy (none here: 503), and so does anyone with auth off.
	for name, g := range map[string]struct {
		g   *GrantAPI
		ctx context.Context
	}{
		"owner":    {&GrantAPI{}, auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindOwner, Name: "sub", Scope: auth.ScopeOwner})},
		"auth off": {&GrantAPI{AuthOff: true}, context.Background()},
	} {
		_, err := g.g.GrantWorkspacePreview(g.ctx, req)
		var he *apierr.Error
		if !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
			t.Fatalf("%s: %v, want 503", name, err)
		}
	}
}

// The handler's 401 must carry the contract's challenge header.
func TestGrantAPIUnauthorizedCarriesTheChallenge(t *testing.T) {
	resp, _ := (&GrantAPI{}).GrantWorkspacePreview(context.Background(), api.GrantWorkspacePreviewRequestObject{WorkspaceId: "ws1", Port: 3000})
	rec := httptest.NewRecorder()
	if err := resp.VisitGrantWorkspacePreviewResponse(rec); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Bearer realm="mainloop"` || !strings.Contains(string(body), "Authentication required") {
		t.Fatalf("got %d %v %s", rec.Code, rec.Header(), body)
	}
}
