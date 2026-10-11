package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

const (
	// maxRequestBytes caps a proxied request body.
	maxRequestBytes = 10 << 20
	// maxResponseHeaderBytes bounds what a dev server may send before the body.
	maxResponseHeaderBytes = 64 << 10
	// responseHeaderTimeout is how long a dev server may take to start answering (a cold build
	// can be slow). Streaming bodies are not bounded.
	responseHeaderTimeout = 60 * time.Second
	// wakeTTL is how long a confirmed wake is trusted: a page load fires many requests, and each
	// would otherwise ask kagent about the Session.
	wakeTTL = 5 * time.Second
)

// Waker is what the proxy needs from the workspace lifecycle (G5). *workspaces.Service satisfies it.
type Waker interface {
	// WakeForPreview makes sure the workspace is running, touching it if it already is.
	WakeForPreview(ctx context.Context, workspaceID string) error
	// Touch records owner activity, which defers the idle suspend.
	Touch(ctx context.Context, workspaceID string) error
}

// Proxy serves preview hosts.
type Proxy struct {
	Pool    *pgxpool.Pool
	Waker   Waker
	OwnerID string
	Log     *slog.Logger

	base     base
	router   string // host:port of the Substrate router
	atespace string
	connect  time.Duration

	// Timings of the keep-awake leases; New sets production values and tests shorten them.
	LeaseTTL      time.Duration
	RenewEvery    time.Duration
	HTTPLeaseWait time.Duration

	holder string
	rp     *httputil.ReverseProxy
	now    func() time.Time

	mu    sync.Mutex
	awake map[string]time.Time
}

// New builds the proxy from the preview settings.
func New(cfg *config.Config, pool *pgxpool.Pool, w Waker, log *slog.Logger) (*Proxy, error) {
	b, ok := parseBase(cfg.Preview.BaseURL)
	if !ok {
		return nil, errors.New("preview: SUBSTRATE_PREVIEW_BASE_URL has no host")
	}
	router, err := routerHostPort(cfg.Preview.RouterAddress)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		Pool: pool, Waker: w, OwnerID: cfg.Identity.OwnerID, Log: log,
		base: b, router: router, atespace: cfg.Kagent.ActorAtespace, connect: cfg.Preview.ConnectTimeout.Duration(),
		LeaseTTL: 60 * time.Second, RenewEvery: 20 * time.Second, HTTPLeaseWait: 10 * time.Second,
		holder: uuid.NewString(), now: time.Now, awake: map[string]time.Time{},
	}
	p.rp = p.reverseProxy()
	return p, nil
}

// Route sends preview hosts to the proxy and every other request to next. It sits in front of
// the API's Host guard, which would refuse a preview host.
func (p *Proxy) Route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := p.base.parse(r.Host); ok {
			p.serve(w, r, h)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// target is what a preview host resolves to.
type target struct {
	sessionID string
	ports     map[int]bool
}

var errNotAdmitted = errors.New("workspace does not admit this preview")

// resolve reads the owner's workspace. pgx.ErrNoRows means it is not the owner's, is archived or
// has no live Session.
func (p *Proxy) resolve(ctx context.Context, workspaceID string) (target, error) {
	row, err := dbq.New(p.Pool).GetPreviewTarget(ctx, dbq.GetPreviewTargetParams{ID: workspaceID, UserID: p.OwnerID})
	if err != nil {
		return target{}, err
	}
	var decl []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(row.Ports, &decl); err != nil {
		return target{}, fmt.Errorf("workspace ports: %w", err)
	}
	t := target{sessionID: row.KagentSessionID, ports: map[int]bool{}}
	for _, d := range decl {
		t.ports[d.Number] = true
	}
	return t, nil
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, h Host) {
	if refusedMethod(r.Method) {
		refuse(w, http.StatusMethodNotAllowed, "Preview method not allowed")
		return
	}
	upgrade := r.Header.Get("Upgrade") != ""
	if upgrade && (!isWebSocket(r) || r.Method != http.MethodGet) {
		refuse(w, http.StatusBadRequest, "Unsupported protocol upgrade")
		return
	}
	// The Origin boundary comes first: a refused request is not looked up, touched or woken, and
	// never reaches the dev server.
	if (upgrade || !safeMethod(r.Method)) && p.originRefused(r) {
		refuse(w, http.StatusForbidden, "Cross-origin preview request refused")
		return
	}
	t, err := p.resolve(r.Context(), h.WorkspaceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		refuse(w, http.StatusNotFound, "Workspace not found")
		return
	case err != nil:
		p.Log.ErrorContext(r.Context(), "preview target lookup failed", "workspace", h.WorkspaceID, "error", err)
		refuse(w, http.StatusBadGateway, "Preview target unavailable")
		return
	case !t.ports[h.Port]:
		refuse(w, http.StatusForbidden, "Preview port is not declared")
		return
	}
	if r.ContentLength > maxRequestBytes {
		refuse(w, http.StatusRequestEntityTooLarge, "Preview request is too large")
		return
	}
	if err := p.ensureAwake(r.Context(), h.WorkspaceID); err != nil {
		var unc *workspaces.UnconfirmedError
		var rej *workspaces.RejectedError
		switch {
		case errors.As(err, &unc), errors.Is(err, workspaces.ErrBusy):
			// Another operation (an idle suspend, say) is in flight; this clears on its own.
			wakingPage(w)
		case errors.As(err, &rej):
			refuse(w, http.StatusConflict, "Workspace cannot be resumed")
		case errors.Is(err, workspaces.ErrNotFound):
			refuse(w, http.StatusNotFound, "Workspace not found")
		default:
			p.Log.WarnContext(r.Context(), "preview wake failed", "workspace", h.WorkspaceID, "error", err)
			refuse(w, http.StatusBadGateway, "Workspace could not be resumed")
		}
		return
	}

	lease := p.holdLease(h.WorkspaceID, upgrade)
	defer lease.release()
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	p.rp.ServeHTTP(w, r.WithContext(withHop(r.Context(), h)))
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// refusedMethod reports methods refused outright: a browser cannot send them, and a dev server has
// no use for them. CONNECT would otherwise reach the dev server as a tunnel request of its own.
func refusedMethod(m string) bool {
	return m == http.MethodConnect || m == http.MethodTrace || m == "TRACK"
}

func isWebSocket(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// originRefused reports whether the request carries an Origin that is not the preview's own:
// the base URL's scheme with the request's Host. A request with no Origin is a non-browser
// client, which a page cannot make send anything.
func (p *Proxy) originRefused(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return false
	case 1:
		return strings.ToLower(strings.TrimRight(origins[0], "/")) != p.base.scheme+"://"+strings.ToLower(r.Host)
	}
	return true
}

// ensureAwake resumes a suspended workspace and touches a running one. A wake that was just
// confirmed is trusted for wakeTTL; the touch still happens so the idle clock keeps moving.
func (p *Proxy) ensureAwake(ctx context.Context, workspaceID string) error {
	p.mu.Lock()
	until, ok := p.awake[workspaceID]
	p.mu.Unlock()
	if ok && p.now().Before(until) {
		return p.Waker.Touch(ctx, workspaceID)
	}
	if err := p.Waker.WakeForPreview(ctx, workspaceID); err != nil {
		return err
	}
	now := p.now()
	p.mu.Lock()
	for id, t := range p.awake {
		if !now.Before(t) {
			delete(p.awake, id)
		}
	}
	p.awake[workspaceID] = now.Add(wakeTTL)
	p.mu.Unlock()
	return nil
}

func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func wakingPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("<!doctype html><title>Workspace waking up</title>" +
		"<main><h1>Workspace waking up</h1><p>Reload this preview in a few seconds.</p></main>"))
}
