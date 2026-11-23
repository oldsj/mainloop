// Package webfetch fetches one public web page for an agent and turns it into text
// (docs/specs/web-fetch.md). It is its own SSRF guard: the Mainloop backend can reach PostgreSQL,
// the Kubernetes API and the LAN, so every address a fetch connects to, including each redirect
// hop and whatever a name resolves to at that moment, is checked as it is dialed, and anything
// that is not a public unicast address is refused.
package webfetch

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// MaxURL bounds a URL, in bytes.
const MaxURL = 2048

// nonPublic are the ranges no fetch may connect to, beyond what [netip.Addr]'s own predicates
// cover (loopback, private, link-local, multicast, unspecified).
var nonPublic = mustPrefixes(
	"0.0.0.0/8",          // "this network"
	"100.64.0.0/10",      // carrier-grade NAT
	"192.0.0.0/24",       // IETF protocol assignments
	"192.0.2.0/24",       // documentation
	"192.88.99.0/24",     // 6to4 relay anycast
	"198.18.0.0/15",      // benchmarking
	"198.51.100.0/24",    // documentation
	"203.0.113.0/24",     // documentation
	"240.0.0.0/4",        // reserved, and the broadcast address
	"100::/64",           // discard
	"2001::/23",          // IETF protocol assignments (Teredo and others)
	"2001:db8::/32",      // documentation
	"2002::/16",          // 6to4: wraps an IPv4 address
	"64:ff9b:1::/48",     // local-use NAT64
	"fec0::/10",          // deprecated site-local
	"::ffff:0:0:0/96",    // IPv4-translated
	"5f00::/16",          // SRv6 SIDs
	"3fff::/20",          // documentation
	"fc00::/7",           // unique local (also IsPrivate; listed for clarity)
	"ff00::/8",           // multicast
	"169.254.0.0/16",     // link-local, cloud metadata
	"fe80::/10",          // link-local
	"127.0.0.0/8",        // loopback
	"::1/128",            // loopback
	"10.0.0.0/8",         // private
	"172.16.0.0/12",      // private
	"192.168.0.0/16",     // private
	"224.0.0.0/4",        // multicast
	"255.255.255.255/32", // broadcast
)

// nat64 is the well-known NAT64 prefix: its last 32 bits are an IPv4 address, checked as such.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// ParseDeny parses a comma-separated list of CIDRs (MAINLOOP_WEB_FETCH_DENY_CIDRS): ranges an
// operator adds to the refused set, such as a cluster or LAN that uses public addresses.
func ParseDeny(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("webfetch: deny CIDR %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Public reports whether a fetch may connect to addr: a global unicast address outside every
// reserved range and every range in deny. An IPv4-mapped or NAT64 address is judged by the IPv4
// address it carries.
func Public(addr netip.Addr, deny []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.Is6() && nat64.Contains(addr) {
		b := addr.As16()
		return Public(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), deny)
	}
	if addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	for _, p := range deny {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Error is a refusal the caller can show: Code is stable, Msg says what happened.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return "[" + e.Code + "] " + e.Msg }

// Codes of [Error].
const (
	CodeInvalidURL         = "invalid_url"
	CodeBlockedAddress     = "blocked_address"
	CodeTooManyRedirects   = "too_many_redirects"
	CodeUnsupportedContent = "unsupported_content"
	CodeTimeout            = "timeout"
	CodeFetchFailed        = "fetch_failed"
)

func refuse(code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// errBlocked is what the dialer returns for a refused address; Fetch reports it as
// [CodeBlockedAddress].
var errBlocked = errors.New("webfetch: address is not public")

// checkURL accepts an absolute http or https URL on the default ports, with a host, no
// credentials and no more than MaxURL bytes. A literal IP must be public.
func checkURL(u *url.URL, deny []netip.Prefix, extraPort string) *Error {
	switch {
	case len(u.String()) > MaxURL:
		return refuse(CodeInvalidURL, "the URL is longer than %d bytes", MaxURL)
	case u.Scheme != "http" && u.Scheme != "https":
		return refuse(CodeInvalidURL, "only http and https URLs are fetched")
	case u.User != nil:
		return refuse(CodeInvalidURL, "a URL with credentials is not fetched")
	case u.Hostname() == "":
		return refuse(CodeInvalidURL, "the URL has no host")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" && p != extraPort {
		return refuse(CodeInvalidURL, "only ports 80 and 443 are fetched")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !Public(ip, deny) {
		return refuse(CodeBlockedAddress, "%s is not a public address", ip)
	}
	return nil
}
