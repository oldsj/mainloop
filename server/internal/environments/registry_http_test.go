package environments_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/environments/fakeregistry"
)

const (
	cdnURL    = "https://pkg-containers.githubusercontent.com/ghcr1/blobs/config?sig=SIGNEDSECRET"
	challenge = `Bearer realm="https://ghcr.io/token",service="ghcr.io"`
)

var blobDigest = "sha256:" + strings.Repeat("c", 64)

// transport is a fake network. It records every request and answers with a handler.
type transport struct {
	mu      sync.Mutex
	calls   []*http.Request
	handler func(*http.Request) (*http.Response, error)
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls = append(t.calls, r)
	t.mu.Unlock()
	return t.handler(r)
}

func (t *transport) requests() []*http.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*http.Request(nil), t.calls...)
}

func respond(status int, body string, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Add(header[i], header[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// ghcr models GHCR for a config blob: 401 with a challenge, a token, then a 307 to the CDN.
type ghcr struct {
	location      string
	status        int // answer to the authenticated request
	initialStatus int
	tokenStatus   int
	cdn           func(*http.Request) (*http.Response, error)
}

func newConfig() *ghcr {
	return &ghcr{location: cdnURL, status: http.StatusTemporaryRedirect, initialStatus: http.StatusUnauthorized, tokenStatus: http.StatusOK}
}

func (c *ghcr) transport() *transport {
	return &transport{handler: func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Hostname() == "pkg-containers.githubusercontent.com":
			if c.cdn != nil {
				return c.cdn(r)
			}
			return respond(200, "{}"), nil
		case r.URL.Path == "/token":
			return respond(c.tokenStatus, `{"token":"anonymous-token"}`, "Set-Cookie", "token-cookie=secret; Domain=.githubusercontent.com"), nil
		case r.Header.Get("Authorization") == "":
			return respond(c.initialStatus, "", "Www-Authenticate", challenge, "Location", c.location), nil
		}
		return respond(c.status, "", "Location", c.location, "Set-Cookie", "registry-cookie=secret; Domain=.githubusercontent.com"), nil
	}}
}

func readConfig(c *ghcr, registry, kind, reference string) ([]byte, *transport, error) {
	tr := c.transport()
	data, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), registry, "oldsj/dev", kind, reference)
	return data, tr, err
}

func TestCDNOneHopWithoutRegistryCredentials(t *testing.T) {
	for _, target := range []string{cdnURL, strings.Replace(cdnURL, ".com/", ".com:443/", 1)} {
		c := newConfig()
		c.location = target
		data, tr, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
		if err != nil || string(data) != "{}" {
			t.Fatalf("Read(%s) = %q, %v", target, data, err)
		}
		calls := tr.requests()
		if len(calls) != 4 {
			t.Fatalf("calls = %d, want initial, token, authenticated, CDN", len(calls))
		}
		initial, token, authenticated, cdn := calls[0], calls[1], calls[2], calls[3]
		if initial.Header.Get("Authorization") != "" || token.Header.Get("Authorization") != "" {
			t.Error("credentials sent before the exchange")
		}
		if got := authenticated.Header.Get("Authorization"); got != "Bearer anonymous-token" {
			t.Errorf("authenticated Authorization = %q", got)
		}
		for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if cdn.Header.Get(h) != "" {
				t.Errorf("CDN request carries %s", h)
			}
		}
		if got := token.URL.Query().Get("scope"); got != "repository:oldsj/dev:pull" {
			t.Errorf("token scope = %q", got)
		}
		if got := token.URL.Query().Get("service"); got != "ghcr.io" {
			t.Errorf("token service = %q", got)
		}
		for _, r := range calls {
			if r.Header.Get("Accept-Encoding") != "identity" {
				t.Errorf("%s Accept-Encoding = %q", r.URL.Host, r.Header.Get("Accept-Encoding"))
			}
		}
	}
}

func TestCDNDeniedLocationsNeverRequested(t *testing.T) {
	for _, target := range []string{
		"",
		"https://evil.example/config",
		"http://pkg-containers.githubusercontent.com/config",
		"//pkg-containers.githubusercontent.com/config",
		"/config",
		"https://pkg-containers.githubusercontent.com:80/config",
		"https://pkg-containers.githubusercontent.com:444/config",
		"https://pkg-containers.githubusercontent.com:bad/config",
		"https://pkg-containers.githubusercontent.com:/config",
		"https://pkg-containers.githubusercontent.com.evil.example/config",
		"https://pkg-containers.githubusercontent.com./config",
		"https://user@pkg-containers.githubusercontent.com/config",
		"https://user:pass@pkg-containers.githubusercontent.com/config",
		"https://@pkg-containers.githubusercontent.com/config",
		cdnURL + "#fragment",
		cdnURL + "#",
		"https://127.0.0.1/config",
		"https://[::1]/config",
		"https://pkg-containers.githubusercontent.com\t/config",
		"https://pkg-containers.githubusercontent.com/con fig",
		"https://pkg-containers.githubusercontent.com/config\n",
	} {
		t.Run(target, func(t *testing.T) {
			c := newConfig()
			c.location = target
			_, tr, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
			var re *environments.RegistryError
			if !errors.As(err, &re) || (!strings.Contains(re.Message, "Unsupported registry config redirect") && !strings.Contains(re.Message, "unavailable or malformed")) {
				t.Fatalf("error = %v", err)
			}
			if n := len(tr.requests()); n != 3 {
				t.Errorf("calls = %d, want 3 (nothing sent to the denied location)", n)
			}
		})
	}
}

func TestCDNOnlyAuthenticatedGHCR307ForImmutableConfig(t *testing.T) {
	type tc struct {
		name      string
		mod       func(*ghcr)
		kind, ref string
	}
	cases := []tc{
		{name: "manifest kind", kind: "manifests", ref: blobDigest},
		{name: "manifest by tag", kind: "manifests", ref: "latest"},
		{name: "blob by tag", kind: "blobs", ref: "latest"},
		{name: "blob bad digest", kind: "blobs", ref: "sha256:bad"},
		{name: "token 307", kind: "blobs", ref: blobDigest, mod: func(c *ghcr) { c.tokenStatus = 307 }},
		{name: "token 401", kind: "blobs", ref: blobDigest, mod: func(c *ghcr) { c.tokenStatus = 401 }},
		{name: "token 403", kind: "blobs", ref: blobDigest, mod: func(c *ghcr) { c.tokenStatus = 403 }},
	}
	for _, code := range []int{301, 302, 303, 307, 308, 403, 404} {
		cases = append(cases, tc{name: fmt.Sprintf("initial %d", code), kind: "blobs", ref: blobDigest, mod: func(c *ghcr) { c.initialStatus = code }})
	}
	for _, code := range []int{301, 302, 303, 308, 401, 403, 404} {
		cases = append(cases, tc{name: fmt.Sprintf("authenticated %d", code), kind: "blobs", ref: blobDigest, mod: func(c *ghcr) { c.status = code }})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := newConfig()
			if tt.mod != nil {
				tt.mod(c)
			}
			_, tr, err := readConfig(c, "ghcr.io", tt.kind, tt.ref)
			wantRegistryError(t, err, "")
			for _, r := range tr.requests() {
				if r.URL.Host != "ghcr.io" {
					t.Errorf("request to %s", r.URL.Host)
				}
			}
		})
	}
	t.Run("other registry", func(t *testing.T) {
		tr := &transport{handler: func(r *http.Request) (*http.Response, error) {
			switch {
			case r.URL.Path == "/token":
				return respond(200, `{"token":"t"}`), nil
			case r.Header.Get("Authorization") == "":
				return respond(401, "", "Www-Authenticate", `Bearer realm="https://other.example/token",service="other.example"`), nil
			}
			return respond(307, "", "Location", cdnURL), nil
		}}
		_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "other.example", "oldsj/dev", "blobs", blobDigest)
		wantRegistryError(t, err, "HTTP 307")
		if n := len(tr.requests()); n != 3 {
			t.Errorf("calls = %d, want 3", n)
		}
	})
}

func TestAuthOriginNeverFollowed(t *testing.T) {
	for _, realm := range []string{
		cdnURL,
		"http://ghcr.io/token",
		"https://ghcr.io:444/token",
		"https://user@ghcr.io/token",
		"https://ghcr.io/token#fragment",
		"https://ghcr.io.evil.example/token",
		"",
	} {
		t.Run(realm, func(t *testing.T) {
			tr := &transport{handler: func(*http.Request) (*http.Response, error) {
				return respond(401, "", "Www-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, realm)), nil
			}}
			_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "blobs", blobDigest)
			wantRegistryError(t, err, "unsupported authentication origin")
			if n := len(tr.requests()); n != 1 {
				t.Errorf("calls = %d, want 1", n)
			}
		})
	}
	t.Run("non-bearer challenge", func(t *testing.T) {
		tr := &transport{handler: func(*http.Request) (*http.Response, error) {
			return respond(401, "", "Www-Authenticate", `Basic realm="x"`), nil
		}}
		_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
		wantRegistryError(t, err, "private images not supported yet")
		if n := len(tr.requests()); n != 1 {
			t.Errorf("calls = %d, want 1", n)
		}
	})
}

func TestCDNNeverRedirectsOrExchangesAuth(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308, 401, 403, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := newConfig()
			c.cdn = func(*http.Request) (*http.Response, error) {
				return respond(code, "", "Location", cdnURL, "Www-Authenticate", challenge), nil
			}
			_, tr, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
			wantRegistryError(t, err, "")
			if n := len(tr.requests()); n != 4 {
				t.Errorf("calls = %d, want 4 (no follow-up after the CDN)", n)
			}
		})
	}
}

func TestSignedLocationStaysOutOfErrors(t *testing.T) {
	t.Run("caller cancels during the CDN request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		c := newConfig()
		c.cdn = func(r *http.Request) (*http.Response, error) {
			cancel()
			return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: context.Canceled}
		}
		_, err := (&environments.AnonymousRegistry{Transport: c.transport()}).Read(ctx, "ghcr.io", "oldsj/dev", "blobs", blobDigest)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), "SIGNEDSECRET") {
			t.Errorf("error leaks the signed URL: %v", err)
		}
	})
	t.Run("CDN status", func(t *testing.T) {
		c := newConfig()
		c.cdn = func(*http.Request) (*http.Response, error) { return respond(500, ""), nil }
		_, _, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
		wantRegistryError(t, err, "HTTP 500")
		if strings.Contains(err.Error(), "SIGNEDSECRET") {
			t.Errorf("error leaks the signed URL: %v", err)
		}
	})
	t.Run("CDN transport error", func(t *testing.T) {
		c := newConfig()
		c.cdn = func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("dial %s: refused", r.URL) }
		_, _, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
		wantRegistryError(t, err, "unavailable or malformed")
		if strings.Contains(err.Error(), "SIGNEDSECRET") || strings.Contains(err.Error(), "pkg-containers") {
			t.Errorf("error leaks the signed URL: %v", err)
		}
	})
	t.Run("bad location", func(t *testing.T) {
		c := newConfig()
		c.location = "https://evil.example/config?sig=SIGNEDSECRET"
		_, _, err := readConfig(c, "ghcr.io", "blobs", blobDigest)
		wantRegistryError(t, err, "Unsupported registry config redirect")
		if strings.Contains(err.Error(), "SIGNEDSECRET") {
			t.Errorf("error leaks the location: %v", err)
		}
	})
}

// countingBody records whether and how it was read and closed.
type countingBody struct {
	r      io.Reader
	reads  int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) { b.reads++; return b.r.Read(p) }
func (b *countingBody) Close() error               { b.closed = true; return nil }

func TestContentEncodingAndSize(t *testing.T) {
	for _, enc := range []string{"gzip", "br", "deflate", "identity, gzip"} {
		t.Run(enc, func(t *testing.T) {
			body := &countingBody{r: strings.NewReader("{}")}
			tr := &transport{handler: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {enc}}, Body: body}, nil
			}}
			_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
			wantRegistryError(t, err, "identity Content-Encoding")
			if body.reads != 0 || !body.closed {
				t.Errorf("body reads = %d, closed = %v; want unread and closed", body.reads, body.closed)
			}
		})
	}
	t.Run("identity", func(t *testing.T) {
		tr := &transport{handler: func(*http.Request) (*http.Response, error) {
			return respond(200, "{}", "Content-Encoding", " Identity "), nil
		}}
		data, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
		if err != nil || string(data) != "{}" {
			t.Fatalf("Read = %q, %v", data, err)
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		body := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("x"), environments.MaxMetadataBytes+10))}
		tr := &transport{handler: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
		}}
		_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
		wantRegistryError(t, err, "size limit")
		if !body.closed {
			t.Error("body not closed")
		}
	})
	t.Run("exactly the cap", func(t *testing.T) {
		tr := &transport{handler: func(*http.Request) (*http.Response, error) {
			return respond(200, strings.Repeat("x", environments.MaxMetadataBytes)), nil
		}}
		data, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
		if err != nil || len(data) != environments.MaxMetadataBytes {
			t.Fatalf("Read = %d bytes, %v", len(data), err)
		}
	})
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[int]string{
		401: "private images not supported yet",
		403: "private images not supported yet (or image not found)",
		404: "private images not supported yet (or image not found)",
		302: "Registry metadata request failed: HTTP 302",
		500: "Registry metadata request failed: HTTP 500",
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			status := status
			tr := &transport{handler: func(r *http.Request) (*http.Response, error) {
				if status == 401 {
					return respond(401, ""), nil // no challenge: private
				}
				return respond(status, "", "Location", "https://elsewhere.example/"), nil
			}}
			_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
			wantRegistryError(t, err, want)
			if n := len(tr.requests()); n != 1 {
				t.Errorf("calls = %d, want 1 (redirects are never followed)", n)
			}
		})
	}
}

func TestTokenResponses(t *testing.T) {
	serve := func(token string) *transport {
		return &transport{handler: func(r *http.Request) (*http.Response, error) {
			switch {
			case r.URL.Path == "/token":
				return respond(200, token), nil
			case r.Header.Get("Authorization") == "":
				return respond(401, "", "Www-Authenticate", challenge), nil
			}
			return respond(200, "{}"), nil
		}}
	}
	read := func(tr *transport) error {
		_, err := (&environments.AnonymousRegistry{Transport: tr}).Read(context.Background(), "ghcr.io", "oldsj/dev", "manifests", blobDigest)
		return err
	}
	if err := read(serve(`{"token":"a"}`)); err != nil {
		t.Errorf("token field: %v", err)
	}
	if err := read(serve(`{"access_token":"a"}`)); err != nil {
		t.Errorf("access_token field: %v", err)
	}
	for _, bad := range []string{`not json`, `[]`, `null`, `{"token":7}`} {
		wantRegistryError(t, read(serve(bad)), "malformed")
	}
	for _, none := range []string{`{}`, `{"token":""}`, `{"token":null}`, `{"Token":"a"}`} {
		wantRegistryError(t, read(serve(none)), "private images not supported yet")
	}
}

func TestReadBudget(t *testing.T) {
	tr := &transport{handler: func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(time.Minute):
			return respond(200, "{}"), nil
		}
	}}
	reader := &environments.AnonymousRegistry{Transport: tr, ReadBudget: 30 * time.Millisecond}
	_, err := reader.Read(context.Background(), "ghcr.io", "oldsj/dev", "blobs", blobDigest)
	wantRegistryError(t, err, "Registry read elapsed-time budget exceeded")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err = (&environments.AnonymousRegistry{Transport: tr}).Read(ctx, "ghcr.io", "oldsj/dev", "blobs", blobDigest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestValidateOverHTTP runs a whole validation through the anonymous client against a fake
// registry served over the fake transport.
func TestValidateOverHTTP(t *testing.T) {
	store := fakeregistry.New()
	img := store.AddImage("ghcr.io", "oldsj/dev", fakeregistry.Options{Index: true})
	objects := store.Objects()
	tr := &transport{handler: func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return respond(200, `{"token":"t"}`), nil
		}
		if r.Header.Get("Authorization") == "" {
			return respond(401, "", "Www-Authenticate", challenge), nil
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
		n := len(parts)
		key := fakeregistry.Key{Registry: r.URL.Host, Repository: strings.Join(parts[:n-2], "/"), Kind: parts[n-2], Reference: parts[n-1]}
		if data, ok := objects[key]; ok {
			return respond(200, string(data)), nil
		}
		return respond(404, ""), nil
	}}
	val := &environments.Validator{Client: &environments.AnonymousRegistry{Transport: tr}, Allowlist: []string{"ghcr.io"}, Budgets: environments.DefaultBudgets()}
	v, err := val.Validate(context.Background(), img.Reference(), "amd64", "e", "v")
	if err != nil {
		t.Fatal(err)
	}
	if str(v.PlatformManifestDigest) != img.Manifest {
		t.Errorf("manifest = %s, want %s", str(v.PlatformManifestDigest), img.Manifest)
	}
	// Three reads (index, manifest, config), each needing the 401, token and retry round trip.
	if n := len(tr.requests()); n != 9 {
		t.Errorf("requests = %d, want 9", n)
	}
	for _, r := range tr.requests() {
		if u, _ := url.Parse(r.URL.String()); strings.Contains(u.Path, "layers") {
			t.Errorf("layer request %s", u.Path)
		}
	}

	// A missing image reads as private/not found.
	missing := "ghcr.io/oldsj/dev@sha256:" + strings.Repeat("e", 64)
	_, err = val.Validate(context.Background(), missing, "amd64", "e", "v")
	wantRegistryError(t, err, "private images not supported yet (or image not found)")
}
