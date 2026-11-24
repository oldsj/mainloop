package preview

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Preview sign-in (issue #415). A preview host serves nothing without a preview cookie: a
// host-only, HttpOnly cookie that one preview host minted for itself in exchange for a grant, and
// that only that host accepts. The owner API mints the grant (POST
// /workspaces/{id}/previews/{port}/grant) for a signed-in owner and hands it to the preview host in
// the URL fragment, which browsers never send to a server, so no grant lands in an access log.
const (
	// AuthPath is the preview host's own sign-in path. It is answered by the proxy, never
	// forwarded; so is everything else under authPrefix.
	AuthPath   = authPrefix + "preview-auth"
	authPrefix = "/__mainloop/"
	// GrantTTL is how long a grant can be exchanged for a cookie.
	GrantTTL = 60 * time.Second
	// SessionTTL is how long a preview cookie is accepted. A navigation after it ends goes back
	// through the app, which grants again while the owner is signed in.
	SessionTTL = 8 * time.Hour

	kindGrant   = "grant"
	kindSession = "session"
	// maxGrantBody bounds the exchange's form body.
	maxGrantBody = 4 << 10
	// maxNextPath bounds the path a grant returns to.
	maxNextPath = 2048
)

var (
	// ErrPortNotDeclared is a grant for a port the workspace does not declare.
	ErrPortNotDeclared = errors.New("preview port is not declared")
	// ErrBadPath is a grant's return path that is not a path on the preview host.
	ErrBadPath = errors.New("preview path must start with a single /")
	// ErrNotPreviewable is a grant for a workspace that is not the owner's or has no live Session.
	ErrNotPreviewable = errors.New("workspace has no preview")
)

// previewKey derives the preview credential key from the agent token key, separated by purpose
// from every other use of that key.
func previewKey(agentTokenKey []byte) []byte {
	m := hmac.New(sha256.New, agentTokenKey)
	m.Write([]byte("mainloop.preview-auth.v1 key"))
	return m.Sum(nil)
}

// mint returns a credential of kind for the preview host h, valid until exp. It is
// `<payload>.<mac>`, both base64url; the payload names the kind, workspace, port and expiry, so a
// credential is good for one preview host only.
func (p *Proxy) mint(kind string, h Host, exp time.Time) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	payload := strings.Join([]string{"v1", kind, h.WorkspaceID, strconv.Itoa(h.Port),
		strconv.FormatInt(exp.Unix(), 10), base64.RawURLEncoding.EncodeToString(nonce)}, "|")
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + base64.RawURLEncoding.EncodeToString(p.mac(enc))
}

func (p *Proxy) mac(payload string) []byte {
	m := hmac.New(sha256.New, p.authKey)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// valid reports whether tok is an unexpired credential of kind for h.
func (p *Proxy) valid(tok, kind string, h Host) bool {
	_, _, ok := p.parse(tok, kind, h)
	return ok
}

// parse checks that tok is an unexpired credential of kind for h and returns its nonce and expiry.
func (p *Proxy) parse(tok, kind string, h Host) (nonce string, exp time.Time, ok bool) {
	enc, sig, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 512 {
		return "", time.Time{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, p.mac(enc)) {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", time.Time{}, false
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 6 || f[0] != "v1" || f[1] != kind || f[2] != h.WorkspaceID || f[3] != strconv.Itoa(h.Port) || f[5] == "" {
		return "", time.Time{}, false
	}
	unix, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil || !p.now().Before(time.Unix(unix, 0)) {
		return "", time.Time{}, false
	}
	return f[5], time.Unix(unix, 0), true
}

// cookieName is host-only by construction: the __Host- prefix makes the browser refuse it with a
// Domain attribute, which needs Secure, so plain-HTTP development uses a plain name.
func (p *Proxy) cookieName() string {
	if p.base.scheme == "https" {
		return "__Host-mainloop-preview"
	}
	return "mainloop-preview"
}

// signedIn reports whether the request carries a valid preview cookie for h.
func (p *Proxy) signedIn(r *http.Request, h Host) bool {
	for _, c := range r.CookiesNamed(p.cookieName()) {
		if p.valid(c.Value, kindSession, h) {
			return true
		}
	}
	return false
}

// hostName is the preview host for h, with the base URL's port.
func (p *Proxy) hostName(h Host) string {
	name := fmt.Sprintf("%d--%s--preview.%s", h.Port, h.WorkspaceID, p.base.domain)
	if p.base.port != "" {
		name += ":" + p.base.port
	}
	return name
}

// Grant mints a sign-in for the owner's workspace preview on port and returns the URL that
// exchanges it for the preview cookie, then opens next (a path on the preview host). The caller
// has already checked that the request comes from the signed-in owner. Nothing is woken or
// touched.
func (p *Proxy) Grant(ctx context.Context, workspaceID string, port int, next string) (string, time.Time, error) {
	h, next, err := p.admit(ctx, workspaceID, port, next)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := p.now().Add(GrantTTL)
	frag := url.Values{"t": {p.mint(kindGrant, h, exp)}, "next": {next}}
	return p.base.scheme + "://" + p.hostName(h) + AuthPath + "#" + frag.Encode(), exp, nil
}

// admit checks a sign-in for path on the workspace's preview of port: a path on the preview
// host, a host the base URL parses back, the owner's previewable workspace and a declared port.
// It returns the host and the path ("/" when empty). Nothing is woken or touched.
func (p *Proxy) admit(ctx context.Context, workspaceID string, port int, path string) (Host, string, error) {
	if path == "" {
		path = "/"
	}
	if !localPath(path) {
		return Host{}, "", ErrBadPath
	}
	h := Host{Port: port, WorkspaceID: workspaceID}
	if _, ok := p.base.parse(p.hostName(h)); !ok {
		return Host{}, "", ErrNotPreviewable
	}
	t, err := p.resolve(ctx, workspaceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Host{}, "", ErrNotPreviewable
	case err != nil:
		return Host{}, "", err
	case !t.ports[port]:
		return Host{}, "", ErrPortNotDeclared
	}
	return h, path, nil
}

// localPath reports whether next is a path on the same host: one leading slash, not a
// scheme-relative `//host` or `/\host`, no control characters.
func localPath(next string) bool {
	if len(next) > maxNextPath || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return false
	}
	return !strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// serveAuth answers the paths under authPrefix. The Origin check has already run, so a POST here
// comes from this preview's own origin (or a non-browser client).
func (p *Proxy) serveAuth(w http.ResponseWriter, r *http.Request, h Host) {
	if r.URL.Path != AuthPath {
		refuse(w, http.StatusNotFound, "Not found")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		signInPage(w)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxGrantBody)
		if err := r.ParseForm(); err != nil {
			refuse(w, http.StatusUnauthorized, "Preview sign-in expired; open the preview from Mainloop again")
			return
		}
		nonce, exp, ok := p.parse(r.PostForm.Get("t"), kindGrant, h)
		if !ok {
			refuse(w, http.StatusUnauthorized, "Preview sign-in expired; open the preview from Mainloop again")
			return
		}
		// A grant signs in once: a copy of the link that leaked (history, a log, a shared screen)
		// is no good after the owner's own sign-in.
		n, err := dbq.New(p.Pool).UsePreviewGrant(r.Context(), dbq.UsePreviewGrantParams{Nonce: nonce, ExpiresAt: exp})
		if err != nil {
			p.Log.ErrorContext(r.Context(), "preview grant use not recorded", "error", err)
			refuse(w, http.StatusServiceUnavailable, "Preview sign-in is unavailable; try again")
			return
		}
		if n == 0 {
			refuse(w, http.StatusUnauthorized, "This preview sign-in link was already used; open the preview from Mainloop again")
			return
		}
		// Secure whenever previews are HTTPS; only a plain-HTTP development base URL leaves it off,
		// because the browser would drop a Secure cookie there. HttpOnly and SameSite are fixed.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the base URL's scheme, see above.
			Name: p.cookieName(), Value: p.mint(kindSession, h, p.now().Add(SessionTTL)), Path: "/",
			MaxAge: int(SessionTTL.Seconds()), HttpOnly: true, Secure: p.base.scheme == "https", SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		refuse(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// signInPage reads the grant from the fragment, drops it from the address bar and history, posts it
// to this host, and opens the path it names. The script is pinned by a per-response nonce.
func signInPage(w http.ResponseWriter) {
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	nonce := base64.StdEncoding.EncodeToString(nb)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'nonce-"+nonce+"'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>Opening preview</title><main><h1>Opening preview…</h1><p id="m"></p></main>`+
		`<script nonce="%s">(function(){
var f=new URLSearchParams(location.hash.slice(1)),t=f.get("t")||"",n=f.get("next")||"/",m=document.getElementById("m");
history.replaceState(null,"",location.pathname);
if(n.charAt(0)!=="/"||n.charAt(1)==="/"||n.charAt(1)==="\\")n="/";
if(!t){m.textContent="Open this preview from Mainloop.";return}
fetch(%q,{method:"POST",credentials:"same-origin",body:new URLSearchParams({t:t})}).then(function(r){
if(r.ok){location.replace(n)}else{m.textContent="This sign-in has expired. Open the preview from Mainloop again."}
},function(){m.textContent="The preview could not be reached. Reload to try again."});
})();</script>`, html.EscapeString(nonce), AuthPath)
}

// unauthenticated answers a preview request without a valid cookie. A page navigation goes to the
// app, which grants a sign-in for the owner and comes back; anything else (a fetch, a WebSocket
// handshake, curl) is 401. Neither looks the workspace up, wakes it or touches it.
func (p *Proxy) unauthenticated(w http.ResponseWriter, r *http.Request, h Host) {
	if p.frontend != "" && navigation(r) {
		q := url.Values{"path": {r.URL.RequestURI()}}
		target := fmt.Sprintf("%s/previews/%s/%d?%s", p.frontend, url.PathEscape(h.WorkspaceID), h.Port, q.Encode())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	refuse(w, http.StatusUnauthorized, "Preview sign-in required; open the preview from Mainloop")
}

// navigation reports a top-level page load: what a browser marks Sec-Fetch-Mode navigate, or, from
// a browser that sends no fetch metadata, a GET that accepts HTML.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// VisitTTL is how long the preview cookie of a screenshot visit is accepted.
const VisitTTL = 2 * time.Minute

// Visit is what Mainloop's own headless browser needs to open the owner's preview: the page URL
// and the preview cookie that signs it in to that one host.
type Visit struct {
	URL    string
	Cookie *http.Cookie
}

// NewSigner builds a Proxy that only signs visits: it serves nothing and dials no router. The MCP
// process uses it for preview screenshots.
func NewSigner(cfg *config.Config, pool *pgxpool.Pool) (*Proxy, error) {
	b, ok := parseBase(cfg.Preview.BaseURL)
	if !ok {
		return nil, errors.New("preview: SUBSTRATE_PREVIEW_BASE_URL has no host")
	}
	return &Proxy{
		Pool: pool, OwnerID: cfg.Identity.OwnerID, base: b,
		authKey: previewKey([]byte(cfg.Identity.AgentTokenKey.Reveal())), now: time.Now,
	}, nil
}

// Visit signs a visit to path on the workspace's preview of port, for Mainloop's headless browser.
// It refuses exactly what Grant refuses. The cookie is a session cookie like the owner's, accepted
// for VisitTTL; the browser context that holds it is disposed after one capture.
func (p *Proxy) Visit(ctx context.Context, workspaceID string, port int, path string) (Visit, error) {
	h, path, err := p.admit(ctx, workspaceID, port, path)
	if err != nil {
		return Visit{}, err
	}
	exp := p.now().Add(VisitTTL)
	return Visit{
		URL: p.base.scheme + "://" + p.hostName(h) + path,
		// Secure follows the base URL's scheme, as the owner's session cookie does.
		Cookie: &http.Cookie{ //nolint:gosec // G124: Secure follows the base URL's scheme, see above.
			Name: p.cookieName(), Value: p.mint(kindSession, h, exp), Path: "/", Expires: exp,
			HttpOnly: true, Secure: p.base.scheme == "https", SameSite: http.SameSiteLaxMode,
		},
	}, nil
}
