package preview

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/liveactor"
)

const (
	// RunnerPort is the SDK runner's client port on the actor.
	RunnerPort = 7682
	// RunnerSubprotocol is the runner's client event protocol.
	RunnerSubprotocol = "kagent.sdk-runner.v1"
	// liveHost is the outbound host of a live request; the dialer reads the actor from its context.
	liveHost = "live-actor"
	// liveWakeBudget is how long an attach waits for a suspended actor to come back.
	liveWakeBudget = 30 * time.Second
	liveRetryEvery = time.Second
	// liveClientFrameLimit bounds a frame from the browser: only small controls pass.
	liveClientFrameLimit = 64 << 10
	liveWriteTimeout     = 10 * time.Second
)

// liveClientOps are the commands the browser may send the runner directly. Everything else that is
// input (a message, a stop, a model or effort) goes through Mainloop's API, which records it first.
// perm stays until permission answers are paired with the session's requests.
var liveClientOps = map[string]bool{"mode": true, "perm": true}

// liveRESTFor names the API call that replaces a refused client command.
var liveRESTFor = map[string]string{
	"user":      "POST /sessions/{id}/message",
	"interrupt": "POST /sessions/{id}/stop-turn",
	"model":     "POST /sessions/{id}/model",
	"effort":    "POST /sessions/{id}/effort",
}

// ErrLiveNotFound is what a LiveTargets returns for a session the owner cannot open live.
var ErrLiveNotFound = liveactor.ErrNotFound

// ErrLiveWaking is what a LiveTargets returns while the session has no running actor yet.
var ErrLiveWaking = liveactor.ErrWaking

// ErrLiveAsleep is what a LiveTargets returns for a sleeping session the client did not ask to wake.
var ErrLiveAsleep = liveactor.ErrAsleep

// LiveTarget is the actor serving a live session.
type LiveTarget = liveactor.Target

// LiveTargets resolves the owner's session to the actor that runs its SDK runner. wake allows a
// sleeping session; without it a sleeping one is ErrLiveAsleep.
type LiveTargets interface {
	LiveTarget(ctx context.Context, sessionID string, wake bool) (LiveTarget, error)
}

// Live relays GET /sessions/{id}/live to the SDK runner's WebSocket on the session's actor,
// through the Substrate router like the preview proxy. The server bootstraps the runner with a
// token derived from the agent token key, so the token never reaches the browser.
//
// It relays frames, not bytes: Mainloop ends the browser's WebSocket and dials the runner as a
// client. Runner frames pass unchanged; from the browser only liveClientOps pass, and anything else
// is answered with an err frame naming the API call to use, so input reaches the runner only through
// Mainloop's record.
type Live struct {
	Targets        LiveTargets
	Log            *slog.Logger
	FrontendOrigin string
	Dev            bool

	dial     func(ctx context.Context, actor string) (net.Conn, error)
	key      []byte
	retry    time.Duration
	budget   time.Duration
	runner   *websocket.Dialer
	browser  websocket.Upgrader
	boot     *http.Client
	mu       sync.Mutex
	booted   map[string]bool
	livePath *regexp.Regexp
}

// NewLive builds the relay over the proxy's router tunnel.
func NewLive(p *Proxy, targets LiveTargets, tokenKey []byte, frontendOrigin string, dev bool, log *slog.Logger) *Live {
	return newLive(func(ctx context.Context, actor string) (net.Conn, error) {
		return p.connectActor(ctx, actor, RunnerPort)
	}, targets, tokenKey, frontendOrigin, dev, log)
}

func newLive(dial func(context.Context, string) (net.Conn, error), targets LiveTargets, tokenKey []byte,
	frontendOrigin string, dev bool, log *slog.Logger) *Live {
	l := &Live{
		Targets: targets, Log: log, FrontendOrigin: frontendOrigin, Dev: dev,
		dial: dial, key: tokenKey, retry: liveRetryEvery, budget: liveWakeBudget,
		booted: map[string]bool{}, livePath: regexp.MustCompile(`^/sessions/([^/]+)/live$`),
	}
	tr := &http.Transport{
		DialContext:            l.dialLive,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	l.boot = &http.Client{Transport: tr, Timeout: liveWakeBudget + 10*time.Second}
	l.runner = &websocket.Dialer{
		NetDialContext:   l.dialLive,
		HandshakeTimeout: liveWakeBudget + responseHeaderTimeout,
		Subprotocols:     []string{RunnerSubprotocol},
	}
	l.browser = websocket.Upgrader{
		Subprotocols: []string{RunnerSubprotocol},
		// serve checks the Origin before it gets here.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	return l
}

type liveKey struct{}

// Route sends GET /sessions/{id}/live to the relay and everything else to next.
func (l *Live) Route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := l.livePath.FindStringSubmatch(r.URL.Path); m != nil {
			l.serve(w, r, m[1])
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Live) serve(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpx.WriteError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !isWebSocket(r) {
		httpx.WriteError(w, http.StatusBadRequest, "Expected a WebSocket upgrade")
		return
	}
	// A WebSocket upgrade is a GET, which the Origin guard lets through; a page on another origin
	// must not drive the owner's session.
	if o, ok := r.Header["Origin"]; ok && (len(o) != 1 || !httpx.OriginAllowed(o[0], r.Host, l.FrontendOrigin, l.Dev)) {
		httpx.WriteError(w, http.StatusForbidden, "Cross-origin request refused.")
		return
	}
	// Only an owner action wakes a sleeping session: the client asks with wake=1 when the owner
	// sends, answers or presses wake, never on its own reconnect.
	t, err := l.Targets.LiveTarget(r.Context(), sessionID, r.URL.Query().Get("wake") == "1")
	switch {
	case errors.Is(err, ErrLiveAsleep):
		httpx.WriteError(w, http.StatusConflict, "Session is asleep; connect with wake=1 to wake it")
		return
	case errors.Is(err, ErrLiveWaking):
		w.Header().Set("Retry-After", "2")
		httpx.WriteError(w, http.StatusServiceUnavailable, "Session is waking; retry shortly")
		return
	case err == nil && !liveactor.Valid(t.Actor):
		l.Log.ErrorContext(r.Context(), "live target has an invalid actor name", "session", sessionID, "actor", t.Actor)
		httpx.WriteError(w, http.StatusBadGateway, "Live session unavailable")
		return
	case errors.Is(err, ErrLiveNotFound):
		httpx.WriteError(w, http.StatusNotFound, "Session not found or not live")
		return
	case err != nil:
		l.Log.ErrorContext(r.Context(), "live target lookup failed", "session", sessionID, "error", err)
		httpx.WriteError(w, http.StatusBadGateway, "Live session unavailable")
		return
	}
	ctx := context.WithValue(r.Context(), liveKey{}, t)
	if err := l.bootstrap(ctx, t); err != nil {
		var waking *wakingError
		if errors.As(err, &waking) {
			w.Header().Set("Retry-After", "2")
			httpx.WriteError(w, http.StatusServiceUnavailable, "Session is waking; retry shortly")
			return
		}
		l.Log.WarnContext(r.Context(), "live bootstrap failed", "session", sessionID, "error", err)
		httpx.WriteError(w, http.StatusBadGateway, "Live session runner refused bootstrap")
		return
	}
	l.relay(ctx, w, r, t)
}

// relay dials the runner first, so a refusal is still an HTTP answer to the browser, then upgrades
// the browser and pumps frames until either side closes.
func (l *Live) relay(ctx context.Context, w http.ResponseWriter, r *http.Request, t LiveTarget) {
	q := url.Values{"t": {l.token(t.KagentSessionID)}}
	if s := r.URL.Query().Get("since"); s != "" {
		q.Set("since", s)
	}
	u := url.URL{Scheme: "ws", Host: liveHost + ":" + strconv.Itoa(RunnerPort), Path: "/ws", RawQuery: q.Encode()}
	hdr := http.Header{"Host": {upstreamHost + ":" + strconv.Itoa(RunnerPort)}}
	up, resp, err := l.runner.DialContext(ctx, u.String(), hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		var waking *wakingError
		switch {
		case errors.As(err, &waking):
			w.Header().Set("Retry-After", "2")
			httpx.WriteError(w, http.StatusServiceUnavailable, "Session is waking; retry shortly")
		case errors.Is(err, context.Canceled):
			w.WriteHeader(499)
		case resp != nil && resp.StatusCode == http.StatusForbidden:
			// The runner holds a different token (a new /data): bootstrap again next time.
			l.forget(t.Actor)
			httpx.WriteError(w, http.StatusBadGateway, "Live session runner refused the attach")
		default:
			l.Log.WarnContext(ctx, "live relay failed", "error", err)
			httpx.WriteError(w, http.StatusBadGateway, "Live session relay failed")
		}
		return
	}
	defer func() { _ = up.Close() }()
	down, err := l.browser.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered the browser.
	}
	defer func() { _ = down.Close() }()
	down.SetReadLimit(liveClientFrameLimit)

	var mu sync.Mutex // one writer to the browser at a time
	toBrowser := func(kind int, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		_ = down.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
		return down.WriteMessage(kind, data)
	}
	closeWith := func(c *websocket.Conn, err error) {
		code, text := websocket.CloseGoingAway, ""
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			code, text = ce.Code, ce.Text
		}
		if code == websocket.CloseNoStatusReceived || code == websocket.CloseAbnormalClosure {
			code = websocket.CloseGoingAway
		}
		msg := websocket.FormatCloseMessage(code, text)
		_ = c.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
	}

	done := make(chan struct{}, 2)
	go func() { // runner -> browser, unchanged
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := up.ReadMessage()
			if err != nil {
				mu.Lock()
				closeWith(down, err)
				mu.Unlock()
				return
			}
			if err := toBrowser(kind, data); err != nil {
				return
			}
		}
	}()
	go func() { // browser -> runner, controls only
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := down.ReadMessage()
			if err != nil {
				closeWith(up, err)
				return
			}
			if refusal := refuseClientFrame(kind, data); refusal != nil {
				if err := toBrowser(websocket.TextMessage, refusal); err != nil {
					return
				}
				continue
			}
			_ = up.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
			if err := up.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}()
	<-done
}

// refuseClientFrame returns the err frame that answers a browser frame the runner must not get, or
// nil to forward it.
func refuseClientFrame(kind int, data []byte) []byte {
	var c struct {
		K  string `json:"k"`
		ID any    `json:"id,omitempty"`
	}
	if kind == websocket.TextMessage && json.Unmarshal(data, &c) == nil && liveClientOps[c.K] {
		return nil
	}
	text := "the live channel takes only the permission mode and permission answers"
	if call, ok := liveRESTFor[c.K]; ok {
		text = fmt.Sprintf("send %s through Mainloop: %s", c.K, call)
	}
	out, _ := json.Marshal(map[string]any{"k": "err", "text": text, "ref": c.ID})
	return out
}

// token is the runner token for a kagent Session: stable across restarts of this server, never
// stored, never sent to the browser.
func (l *Live) token(sid string) string {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte("kagent.sdk-runner.v1 token\x00" + sid))
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Live) forget(actor string) {
	l.mu.Lock()
	delete(l.booted, actor)
	l.mu.Unlock()
}

// bootstrap sets the runner's token on first attach (trust on first use). 409 means a token is
// already set; if it is not ours, the WebSocket answers 403. A new runtime generation is a new
// actor with a new /data, so it is bootstrapped again. ctx carries the target for the dialer.
func (l *Live) bootstrap(ctx context.Context, t LiveTarget) error {
	sid := t.KagentSessionID
	l.mu.Lock()
	done := l.booted[t.Actor]
	l.mu.Unlock()
	if done {
		return nil
	}
	u := "http://" + liveHost + ":" + strconv.Itoa(RunnerPort) + "/bootstrap"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Host = upstreamHost + ":" + strconv.Itoa(RunnerPort)
	req.Header.Set("x-runner-token", l.token(sid))
	resp, err := l.boot.Do(req)
	if err != nil {
		var waking *wakingError
		if errors.As(err, &waking) {
			return waking
		}
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("runner bootstrap on %s returned HTTP %d: %q", t.Actor, resp.StatusCode, body)
	}
	l.mu.Lock()
	l.booted[t.Actor] = true
	l.mu.Unlock()
	return nil
}

// dialLive tunnels to the actor the request's context names, retrying while the router wakes it.
// Nothing of the request has been sent before the tunnel opens, so a retry is safe.
func (l *Live) dialLive(ctx context.Context, _, addr string) (net.Conn, error) {
	t, ok := ctx.Value(liveKey{}).(LiveTarget)
	if !ok || t.Actor == "" {
		return nil, fmt.Errorf("live: no actor for dial address %q", addr)
	}
	deadline := time.Now().Add(l.budget)
	var last error
	for {
		conn, err := l.dial(ctx, t.Actor)
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().Add(l.retry).After(deadline) {
			return nil, &wakingError{last}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(l.retry):
		}
	}
}
