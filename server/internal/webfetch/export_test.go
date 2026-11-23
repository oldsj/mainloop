package webfetch

import (
	"net/netip"
	"strconv"
)

// AllowOnly makes f connect to ap, a local test server, as if it were public. Every other address
// still meets the real check, so a redirect elsewhere is judged as in production.
func AllowOnly(f *Fetcher, ap netip.AddrPort) {
	f.testPort = strconv.Itoa(int(ap.Port()))
	f.allow = func(got netip.AddrPort) bool {
		if got == ap {
			return true
		}
		return (got.Port() == 80 || got.Port() == 443) && Public(got.Addr(), f.Deny)
	}
}
