// Package preview is the owner-scoped reverse proxy for workspace dev servers. A request whose
// Host is `<port>--<workspace>--preview.<base domain>` is checked against the owner's workspace
// and its declared ports, woken if suspended, and relayed through a CONNECT tunnel to the
// workspace's actor. Everything else falls through to the API.
package preview

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// previewLabel is the one DNS label directly under the base domain: `<port>--<workspace>--preview`.
var previewLabel = regexp.MustCompile(`^([1-9][0-9]{0,4})--([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)--preview$`)

const maxDNSLabel = 63

// Host is a parsed preview host.
type Host struct {
	Port        int
	WorkspaceID string
}

// base is the wildcard domain previews sit under.
type base struct {
	scheme string
	domain string // lower-case, no trailing dot
	port   string // "" when the base URL has none
}

// ParseHost parses a Host header against the base URL's domain. The label must sit directly
// under the domain, so `a.<label>.<domain>` is not a preview, and the port must be the base
// URL's.
func ParseHost(hostHeader string, baseURL string) (Host, bool) {
	b, ok := parseBase(baseURL)
	if !ok {
		return Host{}, false
	}
	return b.parse(hostHeader)
}

func parseBase(baseURL string) (base, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return base{}, false
	}
	return base{
		scheme: strings.ToLower(u.Scheme),
		domain: strings.TrimSuffix(strings.ToLower(u.Hostname()), "."),
		port:   u.Port(),
	}, true
}

func (b base) parse(hostHeader string) (Host, bool) {
	if hostHeader == "" || strings.ContainsAny(hostHeader, "@/?#\\ \t\r\n") {
		return Host{}, false
	}
	u, err := url.Parse("//" + hostHeader)
	if err != nil || u.User != nil {
		return Host{}, false
	}
	if u.Port() != b.port {
		return Host{}, false
	}
	name := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	label, ok := strings.CutSuffix(name, "."+b.domain)
	if !ok || len(label) > maxDNSLabel {
		return Host{}, false
	}
	m := previewLabel.FindStringSubmatch(label)
	if m == nil {
		return Host{}, false
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port > 65535 {
		return Host{}, false
	}
	return Host{Port: port, WorkspaceID: m[2]}, true
}
