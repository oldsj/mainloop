package gitroute_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/gitroute"
)

func TestHostAllowed(t *testing.T) {
	allowed := map[string]bool{"git.example.test": true}
	for _, tc := range []struct {
		host string
		dev  bool
		want bool
	}{
		{"git.example.test", false, true},
		{"GIT.Example.Test:8080", false, true},
		{"git.example.test.", false, true},
		{"evil.example.test", false, false},
		{"git.example.test.evil.test", false, false},
		{"", false, false},
		{"user@git.example.test", false, false},
		{"git.example.test/x", false, false},
		{"git.example.test:notaport", false, false},
		{"localhost", false, false},
		{"127.0.0.1:8080", false, false},
		{"localhost", true, true},
		{"localhost:9000", true, true},
		{"127.0.0.1", true, true},
		{"[::1]:9000", true, true},
		{"::1", true, false},
		{"evil.example.test", true, false},
	} {
		if got := gitroute.HostAllowed(tc.host, allowed, tc.dev); got != tc.want {
			t.Errorf("HostAllowed(%q, dev=%t) = %t, want %t", tc.host, tc.dev, got, tc.want)
		}
	}
}

func TestRoute(t *testing.T) {
	all := []string{gitroute.InfoRefs, gitroute.UploadPack, gitroute.ReceivePack}
	for _, tc := range []struct {
		name      string
		url       string
		endpoints []string
		repo      string // "" means refused
		endpoint  string
	}{
		{"discovery", "/oldsj/alpha.git/info/refs?service=x", all, "oldsj/alpha", "info/refs"},
		{"upload-pack", "/oldsj/alpha.git/git-upload-pack", all, "oldsj/alpha", "git-upload-pack"},
		{"receive-pack", "/oldsj/alpha.git/git-receive-pack", all, "oldsj/alpha", "git-receive-pack"},
		{"dots and dashes", "/old-sj/al.pha_b-1.git/info/refs", all, "old-sj/al.pha_b-1", "info/refs"},
		{"endpoint not listed", "/oldsj/alpha.git/git-upload-pack", []string{gitroute.InfoRefs, gitroute.ReceivePack}, "", ""},
		{"receive-pack not listed", "/oldsj/alpha.git/git-receive-pack", []string{gitroute.InfoRefs, gitroute.UploadPack}, "", ""},
		{"no endpoints listed", "/oldsj/alpha.git/info/refs", nil, "", ""},
		{"unknown endpoint", "/oldsj/alpha.git/HEAD", all, "", ""},
		{"no .git", "/oldsj/alpha/info/refs", all, "", ""},
		{"doubled suffix", "/oldsj/alpha.git.git/info/refs", all, "", ""},
		{"upper-case suffix", "/oldsj/alpha.GIT.git/info/refs", all, "", ""},
		{"encoded dot", "/oldsj/alpha%2e.git/info/refs", all, "", ""},
		{"encoded slash", "/oldsj%2Falpha.git/info/refs", all, "", ""},
		{"encoded letter", "/old%73j/alpha.git/info/refs", all, "", ""},
		{"traversal", "/oldsj/../alpha.git/info/refs", all, "", ""},
		{"extra segment", "/x/oldsj/alpha.git/info/refs", all, "", ""},
		{"trailing slash", "/oldsj/alpha.git/info/refs/", all, "", ""},
		{"double slash", "//oldsj/alpha.git/info/refs", all, "", ""},
		{"underscore owner", "/old_sj/alpha.git/info/refs", all, "", ""},
		{"root", "/", all, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.ParseRequestURI(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			repo, endpoint, ok := gitroute.Route(u, tc.endpoints...)
			if ok != (tc.repo != "") {
				t.Fatalf("ok = %t, want %t", ok, tc.repo != "")
			}
			if ok && (repo.FullName() != tc.repo || endpoint != tc.endpoint) {
				t.Fatalf("got %s %s, want %s %s", repo.FullName(), endpoint, tc.repo, tc.endpoint)
			}
		})
	}
}

func TestRouteRawPathIsRefused(t *testing.T) {
	u := &url.URL{Path: "/oldsj/alpha.git/info/refs", RawPath: "/oldsj/alpha.git/info%2Frefs"}
	if _, _, ok := gitroute.Route(u, gitroute.InfoRefs); ok {
		t.Fatal("a path with a distinct raw spelling was routed")
	}
}

func TestCheckHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		want string
	}{
		{"none", http.Header{}, ""},
		{"ordinary", http.Header{"Authorization": {"Bearer x"}, "User-Agent": {"git/2.45"}, "Accept": {"*/*"}, "Git-Protocol": {"version=2"}}, ""},
		{"x-forwarded-for", http.Header{"X-Forwarded-For": {"10.0.0.1"}}, "routing_header"},
		{"x-forwarded-proto", http.Header{"X-Forwarded-Proto": {"https"}}, "routing_header"},
		{"underscore forwarded", http.Header{"X_forwarded_for": {"10.0.0.1"}}, "routing_header"},
		{"lower-case underscore forwarded", http.Header{"x_forwarded_for": {"10.0.0.1"}}, "routing_header"},
		{"x-real-ip", http.Header{"X-Real-Ip": {"10.0.0.1"}}, "routing_header"},
		{"via", http.Header{"Via": {"1.1 proxy"}}, "routing_header"},
		{"x-envoy-external-address", http.Header{"X-Envoy-External-Address": {"10.0.0.1"}}, "routing_header"},
		{"x-envoy-original-path", http.Header{"X-Envoy-Original-Path": {"/"}}, "routing_header"},
		{"x-actor-id", http.Header{"X-Actor-Id": {"a"}}, "routing_header"},
		{"x-actor-identity", http.Header{"X-Actor-Identity": {"a"}}, "routing_header"},
		{"x-session-id", http.Header{"X-Session-Id": {"s"}}, "routing_header"},
		{"forwarded", http.Header{"Forwarded": {"for=1.2.3.4"}}, "routing_header"},
		{"upgrade", http.Header{"Upgrade": {"websocket"}}, "routing_header"},
		{"proxy-authorization", http.Header{"Proxy-Authorization": {"Basic eDp5"}}, "routing_header"},
		{"x-http-method-override", http.Header{"X-Http-Method-Override": {"POST"}}, "routing_header"},
		{"x-original-url", http.Header{"X-Original-Url": {"/"}}, "routing_header"},
		{"x-rewrite-url", http.Header{"X-Rewrite-Url": {"/"}}, "routing_header"},
		{"upper-case name", http.Header{"VIA": {"proxy"}}, "routing_header"},
		{"duplicate", http.Header{"Accept": {"a", "b"}}, "duplicate_header"},
		{"duplicate routing header", http.Header{"Via": {"a", "b"}}, "duplicate_header"},
		{"empty value list", http.Header{"Accept": {}}, "duplicate_header"},
		{"line feed in value", http.Header{"Git-Protocol": {"version=2\nX-Evil: 1"}}, "routing_header"},
		{"carriage return in value", http.Header{"Accept": {"a\rb"}}, "routing_header"},
		{"NUL in value", http.Header{"Accept": {"a\x00b"}}, "routing_header"},
		{"near miss: x-forward", http.Header{"X-Forward": {"a"}}, ""},
		{"near miss: x-forwarded", http.Header{"X-Forwarded": {"a"}}, ""},
		{"near miss: x-env", http.Header{"X-Env": {"a"}}, ""},
		{"near miss: vi", http.Header{"Vi": {"a"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitroute.CheckHeaders(tc.h); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContentEncoding(t *testing.T) {
	for _, tc := range []struct {
		value    string
		gzip, ok bool
	}{
		{"", false, true},
		{"identity", false, true},
		{"gzip", true, true},
		{"GZIP", false, false},
		{"Identity", false, false},
		{"br", false, false},
		{"deflate", false, false},
		{"gzip, br", false, false},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Content-Encoding", tc.value)
		}
		if gzip, ok := gitroute.ContentEncoding(h); gzip != tc.gzip || ok != tc.ok {
			t.Errorf("ContentEncoding(%q) = %t, %t; want %t, %t", tc.value, gzip, ok, tc.gzip, tc.ok)
		}
	}
}

func TestGitProtocol(t *testing.T) {
	for _, tc := range []struct {
		value, version string
		ok             bool
	}{
		{"", "", true},
		{"version=0", "", true},
		{"version=1", "version=1", true},
		{"version=2", "version=2", true},
		{"version=3", "", false},
		{"version=2:object-format=sha256", "", false},
		{"VERSION=2", "", false},
		{" version=2", "", false},
	} {
		h := http.Header{}
		if tc.value != "" {
			h["Git-Protocol"] = []string{tc.value}
		}
		if version, ok := gitroute.GitProtocol(h); version != tc.version || ok != tc.ok {
			t.Errorf("GitProtocol(%q) = %q, %t; want %q, %t", tc.value, version, ok, tc.version, tc.ok)
		}
	}
}

func TestFraming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		te        []string
		length    int64
		discovery bool
		want      bool
	}{
		{"post with length", nil, 10, false, true},
		{"post with no body", nil, 0, false, true},
		{"post chunked", []string{"chunked"}, -1, false, true},
		{"post unknown length", nil, -1, false, true},
		{"post chunked and length", []string{"chunked"}, 10, false, false},
		{"post chunked twice", []string{"chunked", "chunked"}, -1, false, false},
		{"post other coding", []string{"gzip"}, -1, false, false},
		{"post gzip then chunked", []string{"gzip", "chunked"}, -1, false, false},
		{"discovery empty", nil, 0, true, true},
		{"discovery unknown length", nil, -1, true, true},
		{"discovery with body", nil, 5, true, false},
		{"discovery chunked", []string{"chunked"}, -1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(""))
			r.TransferEncoding, r.ContentLength = tc.te, tc.length
			if got := gitroute.Framing(r, tc.discovery); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}
