package privileged

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const unifiTestKey = "fictional-unifi-key-0123456789abcdef"

// unifiConsole is a console on loopback with its own self-signed certificate. It records each
// request it received.
type unifiConsole struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []*http.Request
	body []string
}

func (c *unifiConsole) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func newUniFiConsole(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *unifiConsole {
	t.Helper()
	c := &unifiConsole{}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.seen, c.body = append(c.seen, r), append(c.body, string(b))
		c.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func certPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// otherCertificatePEM is a self-signed certificate no test console serves (httptest's servers all
// share one).
func otherCertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "console.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// unifiFixture is an ExecUniFi for the console, with the key and the console's certificate pinned.
func unifiFixture(t *testing.T, c *unifiConsole) ExecUniFi {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{unifiAPIKeyFile: unifiTestKey + "\n", unifiCertificateFile: certPEM(c.srv)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ExecUniFi{Address: netip.MustParseAddrPort(c.srv.Listener.Addr().String()), Dir: dir}
}

// The executor accepts the console's pinned certificate, then sends the request with the API key
// and the exact body; it follows no redirect.
func TestExecUniFiSendsToThePinnedConsole(t *testing.T) {
	c := newUniFiConsole(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxy/network/api/s/default/stat/health" {
			http.Redirect(w, r, "https://login.example.com/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"meta":{"rc":"ok"}}`)
	})
	e := unifiFixture(t, c)
	ctx := context.Background()
	hello, err := e.Hello(ctx)
	if want := certificateFingerprint(sha256.Sum256(c.srv.Certificate().Raw)); err != nil || hello.Certificate != want || hello.Address != e.Address.String() {
		t.Fatalf("Hello = %+v, %v; want %s", hello, err, want)
	}
	res, err := e.Do(ctx, UniFiRequest{Method: "PUT", Path: "/proxy/network/api/s/default/rest/networkconf/abc?x=1", Body: []byte(`{"vlan":61}`), Expect: &hello})
	if err != nil || res.Status != 200 || string(res.Body) != `{"meta":{"rc":"ok"}}` || res.APIKey != unifiTestKey {
		t.Fatalf("Do = %+v, %v", res, err)
	}
	r := c.seen[0]
	if r.Method != "PUT" || r.URL.RequestURI() != "/proxy/network/api/s/default/rest/networkconf/abc?x=1" || r.Header.Get("X-API-KEY") != unifiTestKey ||
		r.Header.Get("Content-Type") != "application/json" || c.body[0] != `{"vlan":61}` {
		t.Fatalf("the console saw %s %s %v %q", r.Method, r.URL, r.Header, c.body[0])
	}
	res, err = e.Do(ctx, UniFiRequest{Method: "GET", Path: "/proxy/network/api/s/default/stat/health"})
	if err != nil || res.Status != http.StatusFound || c.requests() != 2 {
		t.Fatalf("a redirect: %+v, %v after %d requests", res, err, c.requests())
	}
}

// A console with another certificate, or not the one the owner saw, gets nothing: no request, so
// no API key.
func TestExecUniFiRefusesAnotherCertificate(t *testing.T) {
	c := newUniFiConsole(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	e := unifiFixture(t, c)
	ctx := context.Background()
	hello, err := e.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moved := hello
	moved.Certificate = "SHA256:b3RoZXI"
	var nothing *NothingRan
	if _, err := e.Do(ctx, UniFiRequest{Method: "POST", Path: "/proxy/network/api/s/default/cmd/devmgr", Expect: &moved}); !errors.As(err, &nothing) {
		t.Fatalf("Do with another expected certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, unifiCertificateFile), []byte(otherCertificatePEM(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	var r *refusal
	if _, err := e.Hello(ctx); !errors.As(err, &r) || !strings.Contains(r.msg, "is not the pinned one (item mainloop-executor-unifi, field certificate)") {
		t.Fatalf("Hello: %v", err)
	}
	if _, err := e.Do(ctx, UniFiRequest{Method: "POST", Path: "/proxy/network/api/s/default/cmd/devmgr"}); !errors.As(err, &nothing) {
		t.Fatalf("Do: %v", err)
	}
	if c.requests() != 0 {
		t.Fatalf("the console saw %d requests", c.requests())
	}
}

// A missing or malformed credential names the item and field, never the key.
func TestExecUniFiCredential(t *testing.T) {
	c := newUniFiConsole(t, func(http.ResponseWriter, *http.Request) {})
	for name, tc := range map[string]struct {
		file, text, want string
	}{
		"no key":          {unifiAPIKeyFile, "", "credential_missing: the executor has no api-key for the UniFi console (1Password item mainloop-executor-unifi, field api-key)"},
		"key with spaces": {unifiAPIKeyFile, "two words in a key", "(item mainloop-executor-unifi, field api-key) is not a key"},
		"no certificate":  {unifiCertificateFile, "", "field certificate"},
		"not PEM":         {unifiCertificateFile, "SHA256:abc", "has no PEM CERTIFICATE block"},
	} {
		t.Run(name, func(t *testing.T) {
			e := unifiFixture(t, c)
			if err := os.WriteFile(filepath.Join(e.Dir, tc.file), []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := e.Hello(context.Background())
			var r *refusal
			if !errors.As(err, &r) || !strings.Contains(r.msg, tc.want) || strings.Contains(r.msg, "two words") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A connection that breaks after the request was sent is not "nothing sent": the console may have
// acted on it.
func TestExecUniFiBreakAfterSending(t *testing.T) {
	c := newUniFiConsole(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	e := unifiFixture(t, c)
	_, err := e.Do(context.Background(), UniFiRequest{Method: "POST", Path: "/proxy/network/api/s/default/cmd/devmgr", Body: []byte(`{"cmd":"restart"}`)})
	var nothing *NothingRan
	if err == nil || errors.As(err, &nothing) || !strings.Contains(err.Error(), "before the console answered") || c.requests() != 1 {
		t.Fatalf("err = %v after %d requests", err, c.requests())
	}
}

// A console the executor cannot reach says why without its address.
func TestExecUniFiUnreachable(t *testing.T) {
	c := newUniFiConsole(t, func(http.ResponseWriter, *http.Request) {})
	e := unifiFixture(t, c)
	e.Dial = func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
	}
	_, err := e.Hello(context.Background())
	var r *refusal
	if !errors.As(err, &r) || r.msg != "cannot reach the UniFi console: no route (the executor's network may not reach it yet)" {
		t.Fatalf("err = %v", err)
	}
	var nothing *NothingRan
	if _, err := e.Do(context.Background(), UniFiRequest{Method: "GET", Path: "/proxy/network/api/s/default/stat/health"}); !errors.As(err, &nothing) ||
		strings.Contains(err.Error(), e.Address.Addr().String()) {
		t.Fatalf("Do: %v", err)
	}
}
