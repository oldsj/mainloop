package httpx

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/oldsj/mainloop/server/internal/config"
)

var (
	localhostOrigin = regexp.MustCompile(`^http://localhost:\d+$`)
	loopbackHosts   = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
)

// HostGuard answers 404 {"detail":"Not Found"} for a Host that is not in allowed. Loopback is
// accepted only when dev is set. Paths in exempt skip the check: Kubernetes probes address the
// pod by IP. Without the guard any name that resolves to the node (wildcard DNS, rebinding)
// would reach the API.
func HostGuard(allowed map[string]bool, dev bool, exempt ...string) func(http.Handler) http.Handler {
	skip := map[string]bool{}
	for _, p := range exempt {
		skip[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !skip[r.URL.Path] && !HostAllowed(r.Host, allowed, dev) {
				WriteError(w, http.StatusNotFound, "Not Found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// HostAllowed reports whether a Host header value names an allowed host, or loopback when dev is set.
func HostAllowed(hostHeader string, allowed map[string]bool, dev bool) bool {
	name, ok := config.Hostname(hostHeader)
	if !ok {
		return false
	}
	return (dev && loopbackHosts[name]) || allowed[name]
}

// OriginGuard refuses with 403 {"detail":"Cross-origin request refused."} a browser write that
// a page on another origin sent. CORS only stops such a page from reading the response; a
// body-less no-cors POST is still sent, and the API has no per-request authentication.
// Browsers always send Origin on a cross-origin write, so a client that sends none (MCP
// actors, scripts, curl) is not affected. Reads and preflights are not checked.
func OriginGuard(frontendOrigin string, dev bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin, present := r.Header["Origin"]
			if present && !safeMethod(r.Method) && !originAllowed(origin[0], r.Host, frontendOrigin, dev) {
				WriteError(w, http.StatusForbidden, "Cross-origin request refused.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func originAllowed(origin, host, frontendOrigin string, dev bool) bool {
	if origin == frontendOrigin {
		return true
	}
	if dev && localhostOrigin.MatchString(origin) {
		return true
	}
	// The API's own origin: scheme-independent, port-sensitive, as in the Python guard. The
	// literal "null" origin and malformed values never match.
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || strings.ContainsAny(origin, " \t") {
		return false
	}
	return host != "" && strings.EqualFold(u.Host, host)
}
