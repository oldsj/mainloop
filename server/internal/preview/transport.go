package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// upstreamHost is the name the dev server sees as its Host, and the CONNECT target the router
// resolves to the actor's local port.
const upstreamHost = "actor-upstream"

// hop is the workspace and port one proxied request is for. The outbound URL carries it as
// `<workspace>.actor-upstream:<port>` so the dialer knows where to tunnel without trusting
// anything from the client.
type hop struct {
	workspaceID string
	port        int
}

type hopKey struct{}

func withHop(ctx context.Context, h Host) context.Context {
	return context.WithValue(ctx, hopKey{}, hop{h.WorkspaceID, h.Port})
}

// wakingError is a tunnel that could not be opened, before any byte of the request was sent, so
// the client can retry.
type wakingError struct{ err error }

func (e *wakingError) Error() string { return "preview tunnel unavailable: " + e.err.Error() }
func (e *wakingError) Unwrap() error { return e.err }

// Private request headers never reach the dev server: credentials, and what the owner's gateway
// adds about who and where the owner is. The agent writes the page and runs the server, so
// anything forwarded is visible to it. A denylist, because apps send their own headers (CSRF
// tokens, X-Requested-With, GraphQL and RPC clients) that an allowlist would break.
var (
	privateHeaders = map[string]bool{
		"authorization": true, "proxy-authorization": true, "cookie": true, "forwarded": true,
		"x-real-ip": true, "x-user-id": true,
	}
	privateHeaderPrefixes = []string{"tailscale-", "x-forwarded-", "x-envoy-"}
)

func privateHeader(name string) bool {
	name = strings.ToLower(name)
	if privateHeaders[name] {
		return true
	}
	for _, p := range privateHeaderPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (p *Proxy) reverseProxy() *httputil.ReverseProxy {
	tr := &http.Transport{
		DialContext: p.dial,
		// One tunnel per request: the owner, Session and port are checked again on every dial,
		// and a pooled tunnel would outlive a replaced Session.
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			h, _ := pr.In.Context().Value(hopKey{}).(hop)
			out := pr.Out
			out.URL.Scheme = "http"
			out.URL.Host = h.workspaceID + "." + upstreamHost + ":" + strconv.Itoa(h.port)
			// Exactly one Host, the upstream one: a client Host would make strict servers answer 400.
			out.Host = upstreamHost + ":" + strconv.Itoa(h.port)
			for name := range out.Header {
				if privateHeader(name) {
					delete(out.Header, name)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// A dev server must not set cookies on the owner's domain: a Domain-scoped Set-Cookie
			// would reach the owner's other apps. Mainloop uses no cookies, so nothing needs one.
			// This runs for the WebSocket 101 as well.
			resp.Header.Del("Set-Cookie")
			return nil
		},
		Transport: tr,
		// Dev servers stream (HMR, SSE); never hold a chunk back.
		FlushInterval: -1,
		ErrorHandler:  p.proxyError,
	}
}

func (p *Proxy) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	var waking *wakingError
	switch {
	case errors.As(err, &tooBig):
		refuse(w, http.StatusRequestEntityTooLarge, "Preview request is too large")
	case errors.Is(err, errNotAdmitted):
		refuse(w, http.StatusNotFound, "Workspace no longer admits preview")
	case errors.As(err, &waking):
		p.Log.InfoContext(r.Context(), "preview tunnel not ready", "error", waking.err)
		wakingPage(w)
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody to answer.
		w.WriteHeader(499)
	default:
		// The request may have been forwarded, so its outcome is unknown and it is not replayed.
		p.Log.WarnContext(r.Context(), "preview upstream failed", "error", err)
		refuse(w, http.StatusBadGateway, "Preview request failed; its outcome is unknown")
	}
}

// dial opens a CONNECT tunnel through the router to the workspace's actor. It re-resolves the
// owner, Session and port on every attempt, and retries a router refusal once (the actor may
// still be waking): nothing of the request has been sent at that point, so a retry cannot
// duplicate application work.
func (p *Proxy) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	h, ok := parseHop(addr)
	if !ok {
		return nil, fmt.Errorf("preview: unexpected dial address %q", addr)
	}
	var last error
	for range 2 {
		conn, err := p.tunnel(ctx, h)
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, errNotAdmitted) || ctx.Err() != nil {
			return nil, err
		}
		last = err
	}
	return nil, &wakingError{last}
}

func parseHop(addr string) (hop, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return hop{}, false
	}
	ws, ok := strings.CutSuffix(host, "."+upstreamHost)
	n, err := strconv.Atoi(port)
	if !ok || err != nil || n < 1 || n > 65535 {
		return hop{}, false
	}
	return hop{ws, n}, true
}

// headerSafe guards values that go into the CONNECT request head.
var headerSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

func (p *Proxy) tunnel(ctx context.Context, h hop) (net.Conn, error) {
	t, err := p.resolve(ctx, h.workspaceID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !t.ports[h.port] {
		return nil, errNotAdmitted
	}
	if err != nil {
		return nil, err
	}
	if !headerSafe.MatchString(t.sessionID) {
		return nil, errors.New("preview: session id is not a valid actor name")
	}
	d := net.Dialer{Timeout: p.connect}
	conn, err := d.DialContext(ctx, "tcp", p.router)
	if err != nil {
		return nil, err
	}
	target := upstreamHost + ":" + strconv.Itoa(h.port)
	req := &http.Request{
		Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target,
		Header: http.Header{"Ate-Target-Actor": {p.atespace + "/session-" + t.sessionID}},
	}
	_ = conn.SetDeadline(time.Now().Add(p.connect))
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("router CONNECT returned HTTP %d", resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn serves bytes the CONNECT response reader already pulled off the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func routerHostPort(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return "", errors.New("preview: router address must be an http origin")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
