package preview

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

func wantWaking(t *testing.T, e *env) {
	t.Helper()
	rec := e.do("GET", wsHost, "/", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waking up") {
		t.Fatalf("got %d %q, want the waking page", rec.Code, rec.Body.String())
	}
	if n := len(e.router.seen()); n != 0 {
		t.Fatalf("router took %d CONNECTs, want none", n)
	}
}

// The tunnel is mutual TLS: the backend verifies the router and presents its pod certificate.
func TestTheTunnelPresentsThePodCertificate(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if got := e.router.seenClients(); !slices.Equal(got, []string{backendSPIFFEID}) {
		t.Fatalf("router saw clients %v, want [%s]", got, backendSPIFFEID)
	}
}

// The kubelet rotates the short-lived pod certificate in place; the next connect uses the new one.
func TestARotatedPodCertificateIsUsedOnTheNextConnect(t *testing.T) {
	e := newEnv(t)
	e.do("GET", wsHost, "/", nil, nil)
	const rotated = "spiffe://cluster.local/ns/mainloop/sa/mainloop-backend-rotated"
	e.router.allowed.Store(rotated)
	e.router.issueClient(t, rotated)
	if rec := e.do("GET", wsHost, "/", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("after rotation: got %d %q", rec.Code, rec.Body.String())
	}
	if got := e.router.seenClients(); len(got) != 2 || got[1] != rotated {
		t.Fatalf("router saw clients %v, want the rotated certificate second", got)
	}
}

// A router whose certificate the servicedns trust bundle does not sign is never sent a request.
func TestAnUntrustedRouterIsNotSentTheRequest(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.router.caPath(), newTestCA(t, "other").pem)
	wantWaking(t, e)
	if got := e.router.seenClients(); len(got) != 0 {
		t.Fatalf("handshakes completed with an untrusted router: %v", got)
	}
}

// The router accepts the CONNECT of a client whose SPIFFE ID is not on its allow-list and then
// closes the tunnel. By then the request has been forwarded, so it is a 502 and is not replayed.
func TestAClientOffTheAllowListGetsABadGateway(t *testing.T) {
	e := newEnv(t)
	e.router.allowed.Store("spiffe://cluster.local/ns/elsewhere/sa/someone")
	rec := e.do("GET", wsHost, "/", nil, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d %q, want 502", rec.Code, rec.Body.String())
	}
	if got := e.router.seenClients(); len(got) != 1 {
		t.Fatalf("router saw %d handshakes, want 1 (no replay after forwarding)", len(got))
	}
}

// Without the projected pod certificate there is nothing to dial with.
func TestAMissingPodCertificateDialsNothing(t *testing.T) {
	e := newEnv(t)
	if err := os.Remove(e.router.credentialPath()); err != nil {
		t.Fatal(err)
	}
	wantWaking(t, e)
	if got := e.router.seenClients(); len(got) != 0 {
		t.Fatalf("router saw clients %v, want none", got)
	}
}
