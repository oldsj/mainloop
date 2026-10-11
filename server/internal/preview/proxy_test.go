package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/testdb"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

const (
	owner     = "owner-1"
	baseHost  = "localhost:8001"
	origin    = "http://" + baseHost
	sessionID = "kagent-sess-1"
)

// fakeWaker records the calls and answers WakeForPreview from a script.
type fakeWaker struct {
	mu      sync.Mutex
	wakes   []string
	touches []string
	wakeErr error
}

func (f *fakeWaker) WakeForPreview(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakes = append(f.wakes, id)
	return f.wakeErr
}

func (f *fakeWaker) Touch(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, id)
	return nil
}

func (f *fakeWaker) counts() (wakes, touches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wakes), len(f.touches)
}

// fakeRouter is a Substrate router stand-in: it accepts CONNECT, records the target actor and
// bridges the tunnel to the dev server at devAddr.
type fakeRouter struct {
	ln      net.Listener
	devAddr atomic.Value // string
	// refuse is how many of the next CONNECTs answer 503.
	refuse  atomic.Int32
	mu      sync.Mutex
	targets []string // "<actor> <connect target>"
	wg      sync.WaitGroup
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRouter{ln: ln}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func() { defer r.wg.Done(); r.handle(c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); r.wg.Wait() })
	return r
}

func (r *fakeRouter) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.targets = append(r.targets, req.Header.Get("Ate-Target-Actor")+" "+req.Host+" "+req.Method)
	r.mu.Unlock()
	if r.refuse.Add(-1) >= 0 {
		_, _ = io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
		return
	}
	dev, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", r.devAddr.Load().(string))
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = dev.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dev, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, dev); done <- struct{}{} }()
	<-done
}

func (r *fakeRouter) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.targets...)
}

func (r *fakeRouter) addr() string { return "http://" + r.ln.Addr().String() }

// env is a seeded owner workspace behind a fake router and dev server.
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	waker  *fakeWaker
	router *fakeRouter
	proxy  *Proxy
	// next gets everything that is not a preview host.
	next http.Handler
	dev  *httptest.Server
	// devHandler serves the dev server; tests replace it before sending.
	devHandler atomic.Value // http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, waker: &fakeWaker{}, router: newFakeRouter(t)}
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dev") })))
	e.dev = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.devHandler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(e.dev.Close)
	e.router.devAddr.Store(e.dev.Listener.Addr().String())

	cfg, err := config.Parse(map[string]string{
		"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test",
		"MAINLOOP_OWNER_ID": owner, "SUBSTRATE_PREVIEW_BASE_URL": origin,
		"SUBSTRATE_ROUTER_ADDRESS": e.router.addr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = testdb.New(t).Pool
	e.proxy, err = New(cfg, e.pool, e.waker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "api") })
	e.seed("ws1", owner, sessionID, `[{"name":"web","number":3000},{"name":"api","number":8080}]`)
	return e
}

// seed inserts a project (once), a session, its agent binding and a workspace row.
func (e *env) seed(id, userID, kagentSession, ports string) {
	e.t.Helper()
	ctx := context.Background()
	project := "proj-" + userID
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', 'n', $3, 'https://x')
		  ON CONFLICT DO NOTHING`, []any{project, userID, "o/" + userID}},
		{`INSERT INTO sessions (id, user_id, project_id, title, description, prompt) VALUES ($1, $2, $3, 't', 'd', 'p')`,
			[]any{id, userID, project}},
		{`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ($1, 'claude', 'agent', NULLIF($2, ''))`,
			[]any{id, kagentSession}},
		{`INSERT INTO workspaces (id, user_id, project_id, repo_url, branch, agent_kind, agent_namespace, agent_name, ports, request_id)
		  VALUES ($1, $2, $3, 'https://x', 'b', 'claude', 'kagent', 'claude', $4::jsonb, $5)`, []any{id, userID, project, ports, "req-" + id}},
	}
	for _, s := range stmts {
		if _, err := e.pool.Exec(ctx, s.sql, s.args...); err != nil {
			e.t.Fatalf("seed %s: %v", id, err)
		}
	}
}

// do sends one request through the proxy's Route handler.
func (e *env) do(method, host, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, "http://"+host+path, body)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.proxy.Route(e.next).ServeHTTP(rec, req)
	return rec
}

const wsHost = "3000--ws1--preview." + baseHost

func TestNonPreviewHostFallsThrough(t *testing.T) {
	e := newEnv(t)
	for _, host := range []string{"api.test", baseHost, "3000--ws1--preview.other.test"} {
		if rec := e.do("GET", host, "/", nil, nil); rec.Body.String() != "api" {
			t.Fatalf("%s: got %q, want the API handler", host, rec.Body.String())
		}
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("a non-preview host opened a tunnel")
	}
}

// Mined: test_preview_proxy forwards the request to the workspace over the tunnel with the
// upstream Host and target actor.
func TestProxiesThroughTunnelToTheWorkspaceActor(t *testing.T) {
	e := newEnv(t)
	var gotHost, gotPath, gotQuery string
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath, gotQuery = r.Host, r.URL.Path, r.URL.RawQuery
		w.Header().Set("X-Dev", "1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})))
	rec := e.do("GET", wsHost, "/a/b?x=1", nil, nil)
	if rec.Code != http.StatusCreated || rec.Body.String() != "hello" || rec.Header().Get("X-Dev") != "1" {
		t.Fatalf("got %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if gotHost != "actor-upstream:3000" || gotPath != "/a/b" || gotQuery != "x=1" {
		t.Fatalf("upstream saw host=%q path=%q query=%q", gotHost, gotPath, gotQuery)
	}
	want := "kagent/session-" + sessionID + " actor-upstream:3000 CONNECT"
	if seen := e.router.seen(); len(seen) != 1 || seen[0] != want {
		t.Fatalf("router saw %v, want [%s]", seen, want)
	}
}

// Mined: the private-header cases (cookie, authorization, x-forwarded-*, tailscale-*).
func TestPrivateHeadersNeverReachTheDevServer(t *testing.T) {
	e := newEnv(t)
	var got http.Header
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	})))
	e.do("GET", wsHost, "/", nil, map[string]string{
		"Authorization": "Bearer s", "Proxy-Authorization": "x", "Cookie": "sid=1", "Forwarded": "for=1.2.3.4",
		"X-Forwarded-For": "1.2.3.4", "X-Forwarded-Host": "h", "X-Forwarded-Proto": "https", "X-Real-Ip": "1.2.3.4",
		"X-User-Id": "owner-1", "Tailscale-User-Login": "me@example.test", "Tailscale-User-Name": "Me",
		"X-Csrf-Token": "keep", "X-Requested-With": "XMLHttpRequest", "Accept": "text/html",
	})
	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "X-Real-Ip", "X-User-Id", "Tailscale-User-Login", "Tailscale-User-Name",
	} {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("%s reached the dev server: %v", name, v)
		}
	}
	for name, want := range map[string]string{"X-Csrf-Token": "keep", "X-Requested-With": "XMLHttpRequest", "Accept": "text/html"} {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q (app headers pass)", name, got.Get(name), want)
		}
	}
}

// Mined: test_preview_proxy_rejects_undeclared_port / foreign owner / unknown workspace.
func TestOwnerAndPortAllowlist(t *testing.T) {
	e := newEnv(t)
	e.seed("ws2", "someone-else", "kagent-sess-2", `[{"name":"web","number":3000}]`)
	e.seed("ws-nosession", owner, "", `[{"name":"web","number":3000}]`)
	if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET archived_at = now() WHERE id = 'ws-nosession'`); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, host string
		want       int
	}{
		{"declared port", "8080--ws1--preview." + baseHost, http.StatusOK},
		{"undeclared port", "9999--ws1--preview." + baseHost, http.StatusForbidden},
		{"other user's workspace", "3000--ws2--preview." + baseHost, http.StatusNotFound},
		{"unknown workspace", "3000--nope--preview." + baseHost, http.StatusNotFound},
		{"archived workspace", "3000--ws-nosession--preview." + baseHost, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := e.do("GET", c.host, "/", nil, nil); rec.Code != c.want {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body.String(), c.want)
			}
		})
	}
	for _, s := range e.router.seen() {
		if !strings.HasSuffix(s, "actor-upstream:8080 CONNECT") && !strings.HasSuffix(s, "actor-upstream:3000 CONNECT") {
			t.Fatalf("a tunnel went to an unexpected target: %s", s)
		}
	}
	if n := len(e.router.seen()); n != 1 {
		t.Fatalf("%d tunnels opened for refused requests, want only the one allowed", n)
	}
	if w, _ := e.waker.counts(); w != 1 {
		t.Fatalf("refused requests woke workspaces: %d wakes", w)
	}
}

func TestOriginRules(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, method, origin string
		want                 int
	}{
		{"safe method with foreign origin", "GET", "https://evil.test", http.StatusOK},
		{"post without origin", "POST", "", http.StatusOK},
		{"post same origin", "POST", "http://" + wsHost, http.StatusOK},
		{"post same origin with slash", "POST", "http://" + wsHost + "/", http.StatusOK},
		{"post foreign origin", "POST", "https://evil.test", http.StatusForbidden},
		{"post sibling preview origin", "POST", "http://8080--ws1--preview." + baseHost, http.StatusForbidden},
		{"post null origin", "DELETE", "null", http.StatusForbidden},
		{"post wrong scheme", "PUT", "https://" + wsHost, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := map[string]string{}
			if c.origin != "" {
				h["Origin"] = c.origin
			}
			if rec := e.do(c.method, wsHost, "/", strings.NewReader("x"), h); rec.Code != c.want {
				t.Fatalf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestRefusedOriginIsNotLookedUpWokenOrTouched(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", wsHost, "/", strings.NewReader("x"), map[string]string{"Origin": "https://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	if w, tc := e.waker.counts(); w != 0 || tc != 0 || len(e.router.seen()) != 0 {
		t.Fatalf("wakes=%d touches=%d tunnels=%d, want none", w, tc, len(e.router.seen()))
	}
	// Even for a workspace that does not exist: the Origin check answers first.
	rec = e.do("POST", "3000--nope--preview."+baseHost, "/", strings.NewReader("x"), map[string]string{"Origin": "https://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown workspace with foreign origin: got %d, want 403", rec.Code)
	}
}

func TestRequestBodyCap(t *testing.T) {
	e := newEnv(t)
	var got atomic.Int64
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		_, _ = io.WriteString(w, "ok")
	})))
	// Declared length over the cap: refused before a tunnel is opened.
	req := httptest.NewRequestWithContext(context.Background(), "POST", "http://"+wsHost+"/", strings.NewReader(""))
	req.Host = wsHost
	req.ContentLength = maxRequestBytes + 1
	rec := httptest.NewRecorder()
	e.proxy.Route(e.next).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || len(e.router.seen()) != 0 {
		t.Fatalf("declared oversize: got %d with %d tunnels", rec.Code, len(e.router.seen()))
	}
	// Unknown length (chunked) that runs over: cut off by the cap, never fully delivered.
	req = httptest.NewRequestWithContext(context.Background(), "POST", "http://"+wsHost+"/", io.LimitReader(zeros{}, maxRequestBytes+1024))
	req.Host = wsHost
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	e.proxy.Route(e.next).ServeHTTP(rec, req)
	if got.Load() > maxRequestBytes {
		t.Fatalf("dev server received %d bytes, over the cap", got.Load())
	}
	// Exactly at the cap passes.
	req = httptest.NewRequestWithContext(context.Background(), "POST", "http://"+wsHost+"/", io.LimitReader(zeros{}, maxRequestBytes))
	req.Host = wsHost
	req.ContentLength = maxRequestBytes
	rec = httptest.NewRecorder()
	e.proxy.Route(e.next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got.Load() != maxRequestBytes {
		t.Fatalf("at the cap: got %d, dev server received %d", rec.Code, got.Load())
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// Mined: test_preview_proxy wakes a suspended workspace first; shows a waking page while kagent
// has not confirmed.
func TestWakeOnPreview(t *testing.T) {
	e := newEnv(t)
	e.do("GET", wsHost, "/", nil, nil)
	if w, _ := e.waker.counts(); w != 1 {
		t.Fatalf("wakes = %d, want 1", w)
	}

	e.waker.wakeErr = &workspaces.UnconfirmedError{Detail: "not yet"}
	e.proxy.awake = map[string]time.Time{}
	rec := e.do("GET", wsHost, "/", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "waking up") {
		t.Fatalf("unconfirmed wake: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}

	e.waker.wakeErr = &workspaces.RejectedError{Detail: "gone"}
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("rejected wake: %d", rec.Code)
	}
	e.waker.wakeErr = workspaces.ErrNotFound
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing workspace: %d", rec.Code)
	}
	e.waker.wakeErr = errors.New("boom")
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("other wake error: %d %q", rec.Code, rec.Body.String())
	}
}

func TestConfirmedWakeIsCachedButStillTouches(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.proxy.now = func() time.Time { return now }
	for range 3 {
		e.do("GET", wsHost, "/", nil, nil)
	}
	if w, tc := e.waker.counts(); w != 1 || tc != 2 {
		t.Fatalf("wakes=%d touches=%d, want 1 wake and 2 touches", w, tc)
	}
	now = now.Add(wakeTTL + time.Second)
	e.do("GET", wsHost, "/", nil, nil)
	if w, _ := e.waker.counts(); w != 2 {
		t.Fatalf("wakes after the cache expired = %d, want 2", w)
	}
}

// A CONNECT refusal before anything was sent is retried once; two in a row show the waking page.
func TestTunnelRetriesOnceThenShowsWakingPage(t *testing.T) {
	e := newEnv(t)
	e.router.refuse.Store(1)
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("one refusal: got %d, want the retry to succeed", rec.Code)
	}
	e.router.refuse.Store(2)
	rec := e.do("GET", wsHost, "/", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waking up") {
		t.Fatalf("two refusals: got %d %q", rec.Code, rec.Body.String())
	}
	if n := len(e.router.seen()); n != 2+2 {
		t.Fatalf("router saw %d CONNECTs, want 4 (a refusal and its retry, then two refusals)", n)
	}
}

// A failure after the request was forwarded is never replayed.
func TestForwardedRequestIsNotReplayed(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close() // die mid-request
	})))
	rec := e.do("POST", wsHost, "/", strings.NewReader("pay"), nil)
	if rec.Code != http.StatusBadGateway || calls.Load() != 1 {
		t.Fatalf("got %d after %d upstream calls, want 502 after 1", rec.Code, calls.Load())
	}
}

// The tunnel is re-admitted at dial time: a workspace archived between the lookup and the dial
// gets no tunnel.
func TestDialRechecksTheWorkspace(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET archived_at = now() WHERE id = 'ws1'`); err != nil {
		t.Fatal(err)
	}
	_, err := e.proxy.dial(context.Background(), "tcp", "ws1.actor-upstream:3000")
	if !errors.Is(err, errNotAdmitted) {
		t.Fatalf("dial after archive: %v", err)
	}
	if _, err := e.proxy.dial(context.Background(), "tcp", "ws1.actor-upstream:4444"); !errors.Is(err, errNotAdmitted) {
		t.Fatalf("dial of an undeclared port: %v", err)
	}
	if _, err := e.proxy.dial(context.Background(), "tcp", "evil.example.com:80"); err == nil {
		t.Fatal("dial of a non-upstream address succeeded")
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("refused dials reached the router")
	}
}

func TestSessionIDCannotInjectIntoTheConnectHead(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_session_id = $1 WHERE session_id = 'ws1'`,
		"x\r\nHost: evil"); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code == http.StatusOK {
		t.Fatal("a session id with control characters was used")
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("tunnel opened for an unsafe session id")
	}
}

// Mined: test_preview_proxy_websocket. The dev server upgrades and echoes raw bytes; the proxy
// must carry both directions and keep the lease while the socket is open.
func TestWebSocketUpgradeIsRelayedAndHoldsALease(t *testing.T) {
	e := newEnv(t)
	var gotUpgrade, gotKey string
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpgrade, gotKey = r.Header.Get("Upgrade"), r.Header.Get("Sec-WebSocket-Key")
		hj, _ := w.(http.Hijacker)
		c, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		line, _ := brw.ReadString('\n')
		_, _ = io.WriteString(c, "echo:"+line)
	})))
	front := httptest.NewServer(e.proxy.Route(e.next))
	t.Cleanup(front.Close)

	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: abc\r\nSec-WebSocket-Version: 13\r\nOrigin: %s\r\n\r\n", wsHost, "http://"+wsHost)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	if gotUpgrade != "websocket" || gotKey != "abc" {
		t.Fatalf("dev server saw Upgrade=%q key=%q", gotUpgrade, gotKey)
	}
	if n := e.activeLeases(); n != 1 {
		t.Fatalf("leases while the socket is open = %d, want 1", n)
	}
	_, _ = io.WriteString(conn, "ping\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo:ping\n" {
		t.Fatalf("echo: %q %v", line, err)
	}
	_ = conn.Close()
	e.waitLeases(0)
}

func TestWebSocketRules(t *testing.T) {
	e := newEnv(t)
	ws := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range ws {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name, method string
		headers      map[string]string
		want         int
	}{
		{"foreign origin", "GET", with(map[string]string{"Origin": "https://evil.test"}), http.StatusForbidden},
		{"null origin", "GET", with(map[string]string{"Origin": "null"}), http.StatusForbidden},
		{"non-websocket upgrade", "GET", map[string]string{"Upgrade": "h2c", "Connection": "Upgrade"}, http.StatusBadRequest},
		{"upgrade without connection token", "GET", map[string]string{"Upgrade": "websocket"}, http.StatusBadRequest},
		{"upgrade by post", "POST", with(nil), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := e.do(c.method, wsHost, "/", nil, c.headers); rec.Code != c.want {
				t.Fatalf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("a refused upgrade opened a tunnel")
	}
}

// activeLeases counts unexpired lease rows.
func (e *env) activeLeases() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM preview_leases WHERE expires_at > now()`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) waitLeases(want int) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.activeLeases() != want {
		if time.Now().After(deadline) {
			e.t.Fatalf("leases = %d, want %d", e.activeLeases(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
