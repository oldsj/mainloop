package preview

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
)

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
type Live struct {
	Targets        LiveTargets
	Log            *slog.Logger
	FrontendOrigin string
	Dev            bool

	dial     func(ctx context.Context, actor string) (net.Conn, error)
	key      []byte
	retry    time.Duration
	budget   time.Duration
	rp       *httputil.ReverseProxy
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
	l.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t, _ := pr.In.Context().Value(liveKey{}).(LiveTarget)
			sid := t.KagentSessionID
			out := pr.Out
			q := url.Values{"t": {l.token(sid)}}
			if s := pr.In.URL.Query().Get("since"); s != "" {
				q.Set("since", s)
			}
			out.URL = &url.URL{Scheme: "http", Host: liveHost + ":" + strconv.Itoa(RunnerPort), Path: "/ws", RawQuery: q.Encode()}
			out.Host = upstreamHost + ":" + strconv.Itoa(RunnerPort)
			for name := range out.Header {
				if privateHeader(name) || strings.EqualFold(name, "Origin") {
					delete(out.Header, name)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode == http.StatusForbidden {
				// The runner holds a different token (a new /data): bootstrap again next time.
				t, _ := resp.Request.Context().Value(liveKey{}).(LiveTarget)
				l.forget(t.Actor)
			}
			return nil
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var waking *wakingError
			switch {
			case errors.As(err, &waking):
				w.Header().Set("Retry-After", "2")
				httpx.WriteError(w, http.StatusServiceUnavailable, "Session is waking; retry shortly")
			case errors.Is(err, context.Canceled):
				w.WriteHeader(499)
			default:
				l.Log.WarnContext(r.Context(), "live relay failed", "error", err)
				httpx.WriteError(w, http.StatusBadGateway, "Live session relay failed")
			}
		},
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
	l.rp.ServeHTTP(w, r.WithContext(ctx))
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
