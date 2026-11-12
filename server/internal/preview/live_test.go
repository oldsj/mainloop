package preview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeTargets map[string]string

func (f fakeTargets) LiveTarget(_ context.Context, id string, wake bool) (LiveTarget, error) {
	if id == "asleep" {
		return LiveTarget{}, ErrLiveWaking
	}
	if id == "sleeping" && !wake {
		return LiveTarget{}, ErrLiveAsleep
	}
	if id == "sleeping" {
		id = "s1"
	}
	if sid, ok := f[id]; ok {
		return LiveTarget{KagentSessionID: sid, Actor: "kagent/session-" + sid + "-ab12"}, nil
	}
	return LiveTarget{}, ErrLiveNotFound
}

// fakeRunner is the runner's client port: PUT /bootstrap (TOFU) and a /ws that echoes every frame
// and records it. A frame "suspend" closes the socket as kagent suspending the actor does.
type fakeRunner struct {
	mu       sync.Mutex
	token    string
	boots    int
	wsQuery  string
	wsHeader http.Header
	got      []string
}

func (f *fakeRunner) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/bootstrap":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.boots++
		if f.token != "" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.token = r.Header.Get("x-runner-token")
	case "/ws":
		f.mu.Lock()
		ok := r.URL.Query().Get("t") == f.token && f.token != ""
		f.wsQuery, f.wsHeader = r.URL.RawQuery, r.Header.Clone()
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		up := websocket.Upgrader{Subprotocols: []string{RunnerSubprotocol}}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.got = append(f.got, string(data))
			f.mu.Unlock()
			if strings.Contains(string(data), "suspend") {
				msg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "actor suspended")
				_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
				return
			}
			if err := conn.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}
}

func newTestLive(t *testing.T, runner http.Handler, dialErr error) (*httptest.Server, *Live) {
	t.Helper()
	up := httptest.NewServer(runner)
	t.Cleanup(up.Close)
	addr := strings.TrimPrefix(up.URL, "http://")
	dial := func(ctx context.Context, actor string) (net.Conn, error) {
		if actor != "kagent/session-kagent-1-ab12" {
			return nil, errors.New("unexpected actor " + actor)
		}
		if dialErr != nil {
			return nil, dialErr
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	l := newLive(dial, fakeTargets{"s1": "kagent-1"}, []byte("key"), "https://app.example", false, slog.New(slog.DiscardHandler))
	l.retry, l.budget = 10*time.Millisecond, 50*time.Millisecond
	front := httptest.NewServer(l.Route(http.NotFoundHandler()))
	t.Cleanup(front.Close)
	return front, l
}

// attach opens a WebSocket to the relay.
func attach(t *testing.T, base, path string, hdr http.Header) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{RunnerSubprotocol}}
	conn, resp, err := d.DialContext(t.Context(), "ws"+strings.TrimPrefix(base, "http")+path, hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("attach %s: %v", path, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Input reaches the runner only through Mainloop's API: the relay answers a message, a stop, a
// model or an effort sent on the socket with an err frame naming the call, and forwards none of
// them. The permission mode and permission answers pass, and the runner's close reaches the browser
// with its code, which is how the view tells a suspend apart.
func TestLiveForwardsOnlyControlsFromTheBrowser(t *testing.T) {
	runner := &fakeRunner{}
	front, _ := newTestLive(t, runner, nil)
	conn := attach(t, front.URL, "/sessions/s1/live", nil)
	refused := map[string]string{
		`{"k":"user","text":"hi","id":"c1"}`: "POST /sessions/{id}/message",
		`{"k":"interrupt"}`:                  "POST /sessions/{id}/stop-turn",
		`{"k":"model","model":"x"}`:          "POST /sessions/{id}/model",
		`{"k":"effort","effort":"max"}`:      "POST /sessions/{id}/effort",
		`not json`:                           "permission mode and permission answers",
	}
	for frame, call := range refused {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatal(err)
		}
		var got struct{ K, Text string }
		if err := conn.ReadJSON(&got); err != nil || got.K != "err" || !strings.Contains(got.Text, call) {
			t.Fatalf("%s: answered %+v, %v; want an err naming %q", frame, got, err, call)
		}
	}
	perm := `{"k":"perm","id":"p1","behavior":"allow"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(perm)); err != nil {
		t.Fatal(err)
	}
	if _, got, err := conn.ReadMessage(); err != nil || string(got) != perm {
		t.Fatalf("perm echo %q %v", got, err)
	}
	if got := runner.received(); !slices.Equal(got, []string{perm}) {
		t.Fatalf("runner received %q, want only the perm answer", got)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"k":"mode","mode":"suspend"}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseGoingAway || ce.Text != "actor suspended" {
		t.Fatalf("close %v, want 1001 actor suspended", err)
	}
}

// upgrade sends a raw WebSocket handshake and returns the response status and headers; on a 101 the
// connection then carries the relayed bytes.
func upgrade(t *testing.T, base, path string, hdr map[string]string) (int, http.Header, net.Conn, *bufio.Reader) {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := "GET " + path + " HTTP/1.1\r\nHost: app.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: " + RunnerSubprotocol + "\r\n"
	for k, v := range hdr {
		req += k + ": " + v + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
	}
	return resp.StatusCode, resp.Header, conn, br
}

func TestLiveRelaysWebSocketAndBootstrapsOnce(t *testing.T) {
	runner := &fakeRunner{}
	front, l := newTestLive(t, runner, nil)
	for i := range 2 {
		conn := attach(t, front.URL, "/sessions/s1/live?since=7", http.Header{
			"Cookie": {"session=secret"}, "Origin": {"https://app.example"},
		})
		if got := conn.Subprotocol(); got != RunnerSubprotocol {
			t.Fatalf("attach %d: subprotocol %q", i, got)
		}
		mode := `{"k":"mode","mode":"acceptEdits"}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(mode)); err != nil {
			t.Fatal(err)
		}
		if _, got, err := conn.ReadMessage(); err != nil || string(got) != mode {
			t.Fatalf("echo %q %v", got, err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.boots != 1 {
		t.Fatalf("bootstraps = %d, want 1", runner.boots)
	}
	if runner.token != l.token("kagent-1") || len(runner.token) < 24 {
		t.Fatalf("runner token %q", runner.token)
	}
	if !strings.Contains(runner.wsQuery, "since=7") {
		t.Fatalf("since not forwarded: %q", runner.wsQuery)
	}
	if runner.wsHeader.Get("Cookie") != "" || runner.wsHeader.Get("Origin") != "" {
		t.Fatalf("private headers forwarded: %v", runner.wsHeader)
	}
}

func TestLiveBootstrapConflictStillAttaches(t *testing.T) {
	runner := &fakeRunner{}
	front, l := newTestLive(t, runner, nil)
	runner.token = l.token("kagent-1") // set by an earlier server process
	if code, _, _, _ := upgrade(t, front.URL, "/sessions/s1/live", nil); code != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", code)
	}
}

func TestLiveRefusals(t *testing.T) {
	front, _ := newTestLive(t, &fakeRunner{}, nil)
	cases := []struct {
		name, path string
		hdr        map[string]string
		want       int
	}{
		{"not live", "/sessions/other/live", nil, http.StatusNotFound},
		{"cross origin", "/sessions/s1/live", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, _, _, _ := upgrade(t, front.URL, c.path, c.hdr); code != c.want {
				t.Fatalf("status %d, want %d", code, c.want)
			}
		})
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/sessions/s1/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET: status %d", resp.StatusCode)
	}
}

func TestLiveWakingIs503(t *testing.T) {
	front, _ := newTestLive(t, &fakeRunner{}, errors.New("router CONNECT returned HTTP 503"))
	for _, path := range []string{"/sessions/s1/live", "/sessions/asleep/live"} {
		code, hdr, _, _ := upgrade(t, front.URL, path, nil)
		if code != http.StatusServiceUnavailable || hdr.Get("Retry-After") == "" {
			t.Fatalf("%s: status %d", path, code)
		}
	}
}

// A sleeping session is refused without wake=1 and never dialled; with it, the relay attaches (the
// router connection is the wake), and wake is not forwarded to the runner.
func TestLiveSleepingNeedsWake(t *testing.T) {
	runner := &fakeRunner{}
	front, _ := newTestLive(t, runner, nil)
	if code, _, _, _ := upgrade(t, front.URL, "/sessions/sleeping/live", nil); code != http.StatusConflict {
		t.Fatalf("without wake: status %d, want 409", code)
	}
	runner.mu.Lock()
	boots := runner.boots
	runner.mu.Unlock()
	if boots != 0 {
		t.Fatalf("a refused attach reached the runner (%d bootstraps)", boots)
	}
	if code, _, _, _ := upgrade(t, front.URL, "/sessions/sleeping/live?wake=1&since=3", nil); code != http.StatusSwitchingProtocols {
		t.Fatalf("with wake: status %d, want 101", code)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if strings.Contains(runner.wsQuery, "wake") || !strings.Contains(runner.wsQuery, "since=3") {
		t.Fatalf("runner query %q", runner.wsQuery)
	}
}
