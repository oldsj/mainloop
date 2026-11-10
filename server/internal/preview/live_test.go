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
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTargets map[string]string

func (f fakeTargets) LiveTarget(_ context.Context, id string) (LiveTarget, error) {
	if id == "asleep" {
		return LiveTarget{}, ErrLiveWaking
	}
	if sid, ok := f[id]; ok {
		return LiveTarget{KagentSessionID: sid, Actor: "kagent/session-" + sid + "-ab12"}, nil
	}
	return LiveTarget{}, ErrLiveNotFound
}

// fakeRunner is the runner's client port: PUT /bootstrap (TOFU) and a /ws that echoes raw bytes.
type fakeRunner struct {
	mu       sync.Mutex
	token    string
	boots    int
	wsQuery  string
	wsHeader http.Header
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
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: " + RunnerSubprotocol + "\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 64)
		n, _ := brw.Read(buf)
		_, _ = conn.Write(buf[:n])
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
		code, hdr, conn, br := upgrade(t, front.URL, "/sessions/s1/live?since=7", map[string]string{
			"Cookie": "session=secret", "Origin": "https://app.example",
		})
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("attach %d: status %d", i, code)
		}
		if got := hdr.Get("Sec-WebSocket-Protocol"); got != RunnerSubprotocol {
			t.Fatalf("subprotocol %q", got)
		}
		if _, err := conn.Write([]byte("frame")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "frame" {
			t.Fatalf("echo %q %v", buf, err)
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
