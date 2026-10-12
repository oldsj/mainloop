// Package gitroute is the request policy the git read transport and the push gate share: which
// Host names are served, which paths are routes, and which headers a client may not send. Each
// server keeps its own endpoints, refusal codes and logging; what counts as a hostile request on
// this boundary is decided here once, so the two cannot drift.
package gitroute

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// Refusal codes the shared checks return. Both servers put them on the wire unchanged.
const (
	CodeHostDenied      = "host_denied"
	CodePathInvalid     = "path_invalid"
	CodeDuplicateHeader = "duplicate_header"
	CodeRoutingHeader   = "routing_header"
	CodeCompression     = "http_compression"
	CodeGitProtocol     = "git_protocol"
	CodeFraming         = "http_framing"
)

// Endpoints a route can name, after "/<owner>/<repo>.git/".
const (
	InfoRefs    = "info/refs"
	UploadPack  = "git-upload-pack"
	ReceivePack = "git-receive-pack"
)

// HostAllowed reports whether a Host header names a served host: one of allowed, or loopback when
// dev is set. It is the parser behind [httpx.HostGuard].
func HostAllowed(hostHeader string, allowed map[string]bool, dev bool) bool {
	return httpx.HostAllowed(hostHeader, allowed, dev)
}

var routePath = regexp.MustCompile(`^/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// Route parses a request URL as a git route. It returns the repository and the endpoint, and false
// unless the path is the one canonical spelling of a route that names one of endpoints: no
// escapes, no doubled ".git" (which ParseRepo would drop), nothing but the listed endpoints.
func Route(u *url.URL, endpoints ...string) (github.Repo, string, bool) {
	path := u.EscapedPath()
	m := routePath.FindStringSubmatch(path)
	if m == nil || strings.Contains(path, "%") || u.RawPath != "" {
		return github.Repo{}, "", false
	}
	endpoint := m[3]
	allowed := false
	for _, e := range endpoints {
		allowed = allowed || e == endpoint
	}
	if !allowed {
		return github.Repo{}, "", false
	}
	repo, err := github.ParseRepo(m[1] + "/" + m[2])
	if err != nil || path != "/"+repo.Owner+"/"+repo.Name+".git/"+endpoint {
		return github.Repo{}, "", false
	}
	return repo, endpoint, true
}

// Headers that name a route, a rewrite, an actor or a proxy hop. A client has no business sending
// them to the transport, and one that does is trying to reach something else.
var routingHeaders = map[string]bool{
	"upgrade": true, "proxy-authorization": true, "x-http-method-override": true,
	"x-original-url": true, "x-rewrite-url": true, "forwarded": true,
	"x-real-ip": true, "via": true,
}

var routingPrefixes = []string{"x-forwarded-", "x_forwarded_", "x-envoy-", "x-actor", "x-session"}

// CheckHeaders refuses a repeated header, a header value with a line break or NUL, and any
// routing, forwarding or proxy header, compared case-insensitively. It returns a refusal code, or
// "" when the headers are acceptable. Names are visited in sorted order so the code is stable.
func CheckHeaders(h http.Header) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		vs := h[k]
		if len(vs) != 1 {
			return CodeDuplicateHeader
		}
		if strings.ContainsAny(vs[0], "\r\n\x00") {
			return CodeRoutingHeader
		}
		name := strings.ToLower(k)
		if routingHeaders[name] {
			return CodeRoutingHeader
		}
		for _, p := range routingPrefixes {
			if strings.HasPrefix(name, p) {
				return CodeRoutingHeader
			}
		}
	}
	return ""
}

// ContentEncoding reads Content-Encoding: gzip reports a gzip body, and ok is false for any
// encoding but none, "identity" or "gzip". The spelling must be exact; whether gzip is served is
// the caller's call.
func ContentEncoding(h http.Header) (gzip, ok bool) {
	switch h.Get("Content-Encoding") {
	case "", "identity":
		return false, true
	case "gzip":
		return true, true
	}
	return false, false
}

// GitProtocol reads Git-Protocol: version is "" for none or version=0, and "version=1" or
// "version=2" as sent; ok is false for any other value. Which versions are served is the caller's
// call.
func GitProtocol(h http.Header) (version string, ok bool) {
	switch p := h.Get("Git-Protocol"); p {
	case "", "version=0":
		return "", true
	case "version=1", "version=2":
		return p, true
	}
	return "", false
}

// Framing reports whether the request's body framing is acceptable: the only transfer coding is a
// single "chunked" and only without a Content-Length, and a discovery request has no body at all.
func Framing(r *http.Request, discovery bool) bool {
	te := r.TransferEncoding
	if len(te) > 1 || (len(te) == 1 && (te[0] != "chunked" || r.ContentLength >= 0)) {
		return false
	}
	return !discovery || (r.ContentLength <= 0 && len(te) == 0)
}
