package privileged

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

// The files of the console's credential, in Dir: a Secret mounted from the 1Password item
// mainloop-executor-unifi.
const (
	unifiAPIKeyFile      = "api-key"
	unifiCertificateFile = "certificate"
	unifiItem            = "mainloop-executor-unifi"
	// maxUniFiResponse bounds what the executor reads of an answer.
	maxUniFiResponse = 1 << 20
	unifiDialTimeout = 15 * time.Second
)

// ExecUniFi reaches the UniFi console over HTTPS with a client of its own: it accepts only the
// console's pinned certificate (consoles serve a self-signed one, so no CA can vouch for it), sends
// the API key only after that check, follows no redirect, and opens one connection per request.
type ExecUniFi struct {
	// Address is the console's IP address and port.
	Address netip.AddrPort
	// Dir holds the credential's files.
	Dir string
	// Dial, if set, opens the TCP connection (tests); otherwise a plain dial.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
}

// unifiCredential is read fresh for each connection, so a rotated Secret takes effect without a
// restart.
type unifiCredential struct {
	apiKey string
	pins   [][sha256.Size]byte
}

func (e ExecUniFi) credential() (unifiCredential, error) {
	root, rootErr := os.OpenRoot(e.Dir)
	if rootErr == nil {
		defer func() { _ = root.Close() }()
	}
	read := func(name string) (string, error) {
		var b []byte
		err := rootErr
		if err == nil {
			b, err = root.ReadFile(name)
		}
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", refuse("credential_missing: the executor has no %s for the UniFi console (1Password item %s, field %s)", name, unifiItem, name)
		}
		return string(b), nil
	}
	var c unifiCredential
	key, err := read(unifiAPIKeyFile)
	if err != nil {
		return c, err
	}
	certs, err := read(unifiCertificateFile)
	if err != nil {
		return c, err
	}
	c.apiKey = strings.TrimSpace(key)
	if len(c.apiKey) < 16 || len(c.apiKey) > 512 || strings.IndexFunc(c.apiKey, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		// Never quote it: say only which field.
		return c, refuse("the UniFi API key (item %s, field %s) is not a key: one line of printable ASCII", unifiItem, unifiAPIKeyFile)
	}
	rest := []byte(certs)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return c, refuse("the UniFi console's certificate (item %s, field %s) does not parse", unifiItem, unifiCertificateFile)
		}
		c.pins = append(c.pins, sha256.Sum256(block.Bytes))
	}
	if len(c.pins) == 0 {
		return c, refuse("the UniFi console's certificate (item %s, field %s) has no PEM CERTIFICATE block", unifiItem, unifiCertificateFile)
	}
	return c, nil
}

func certificateFingerprint(sum [sha256.Size]byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

var errNotPinned = errors.New("not the pinned certificate")

// connect dials the console and completes TLS, accepting only a pinned certificate. Nothing is sent
// on the connection yet. Its refusals name the console, not its address: the agent sees them.
func (e ExecUniFi) connect(ctx context.Context) (*tls.Conn, UniFiHello, unifiCredential, error) {
	hello := UniFiHello{Address: e.Address.String()}
	c, err := e.credential()
	if err != nil {
		return nil, hello, c, err
	}
	ctx, cancel := context.WithTimeout(ctx, unifiDialTimeout)
	defer cancel()
	dial := e.Dial
	if dial == nil {
		var d net.Dialer
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	raw, err := dial(ctx, e.Address.String())
	if err != nil {
		return nil, hello, c, refuse("cannot reach the UniFi console: %s", dialReason(err))
	}
	var presented [sha256.Size]byte
	conn := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The pin below is the whole check: the console's certificate is self-signed, and its name
		// is an address. VerifyConnection runs on every handshake, resumed ones included.
		InsecureSkipVerify: true, //nolint:gosec // G402: verified against the pinned certificate in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errNotPinned
			}
			presented = sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !slices.Contains(c.pins, presented) {
				return errNotPinned
			}
			return nil
		},
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		if errors.Is(err, errNotPinned) {
			return nil, hello, c, refuse("the UniFi console presented a certificate (%s) that is not the pinned one (item %s, field %s)",
				certificateFingerprint(presented), unifiItem, unifiCertificateFile)
		}
		return nil, hello, c, refuse("cannot reach the UniFi console: the TLS handshake failed (%s)", dialReason(err))
	}
	hello.Certificate = certificateFingerprint(presented)
	return conn, hello, c, nil
}

func (e ExecUniFi) Hello(ctx context.Context) (UniFiHello, error) {
	conn, hello, _, err := e.connect(ctx)
	if err != nil {
		return hello, err
	}
	_ = conn.Close()
	return hello, nil
}

// countingWriter counts the bytes that reached the connection.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

func (e ExecUniFi) Do(ctx context.Context, req UniFiRequest) (UniFiResponse, error) {
	conn, hello, c, err := e.connect(ctx)
	if err != nil {
		return UniFiResponse{}, &NothingRan{err}
	}
	defer func() { _ = conn.Close() }()
	if req.Expect != nil && hello != *req.Expect {
		return UniFiResponse{}, &NothingRan{errors.New("the UniFi console's pinned certificate or address changed since the owner saw it")}
	}
	deadline := time.Now().Add(unifiTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, "https://"+e.Address.String()+req.Path, body)
	if err != nil {
		return UniFiResponse{}, &NothingRan{errors.New("the request cannot be built")}
	}
	hreq.Header.Set("X-API-KEY", c.apiKey)
	hreq.Header.Set("Accept", "application/json")
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	hreq.Close = true
	cw := &countingWriter{w: conn}
	bw := bufio.NewWriter(cw)
	err = hreq.Write(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		if cw.n == 0 {
			return UniFiResponse{}, &NothingRan{errors.New("the connection broke before the request was sent (" + dialReason(err) + ")")}
		}
		// Part of it, or all of it, may have reached the console.
		return UniFiResponse{}, errors.New("the connection broke while the request was being sent (" + dialReason(err) + ")")
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), hreq)
	if err != nil {
		return UniFiResponse{}, errors.New("the connection broke before the console answered (" + dialReason(err) + ")")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxUniFiResponse))
	if err != nil {
		return UniFiResponse{}, errors.New("the console's answer was cut off (" + dialReason(err) + ")")
	}
	return UniFiResponse{Status: resp.StatusCode, Body: b, APIKey: c.apiKey}, nil
}
