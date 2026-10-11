package gittransport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
)

type fixedAuth struct{ capability gittransport.Capability }

func (f fixedAuth) Authenticate(_ context.Context, header string) (gittransport.Capability, error) {
	if header != "Bearer ml_good" {
		return gittransport.Capability{}, gittransport.ErrUnauthorized
	}
	return f.capability, nil
}

// stubUpstream records what the proxy asked GitHub for and answers with fixed bytes.
type stubUpstream struct {
	repo   github.Repo
	fetch  github.GitFetch
	body   []byte
	bodyOK error
	reply  io.ReadCloser
	err    error
}

func (s *stubUpstream) GitFetch(_ context.Context, repo github.Repo, f github.GitFetch) (*github.GitResponse, error) {
	s.repo, s.fetch = repo, f
	if f.Body != nil {
		s.body, s.bodyOK = io.ReadAll(f.Body)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.bodyOK != nil {
		return nil, github.ErrUnavailable
	}
	return &github.GitResponse{ContentType: "application/x-git-upload-pack-result", Body: s.reply}, nil
}

func stubServer(up *stubUpstream, maxBody int64) *httptest.Server {
	srv := &gittransport.Server{
		Auth:     fixedAuth{gittransport.Capability{WorkspaceID: "w", CredentialID: "c", Repository: "oldsj/alpha"}},
		Upstream: up, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Dev: true, MaxBody: maxBody,
	}
	return httptest.NewServer(srv.Handler())
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func post(t *testing.T, srv *httptest.Server, body []byte, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", srv.URL+"/oldsj/alpha.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ml_good")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGzipBodyIsInflatedForUpstream(t *testing.T) {
	up := &stubUpstream{reply: io.NopCloser(strings.NewReader("PACK"))}
	srv := stubServer(up, 0)
	defer srv.Close()
	status, body := post(t, srv, gz(t, "0032want abc\n"), map[string]string{"Content-Encoding": "gzip", "Git-Protocol": "version=2"})
	if status != 200 || body != "PACK" {
		t.Fatalf("got %d %q", status, body)
	}
	if string(up.body) != "0032want abc\n" || up.fetch.Protocol != "version=2" || up.fetch.Discovery {
		t.Fatalf("upstream got %q %+v", up.body, up.fetch)
	}
	if up.repo.FullName() != "oldsj/alpha" {
		t.Fatalf("repo = %v", up.repo)
	}
}

func TestBodyLimits(t *testing.T) {
	up := &stubUpstream{reply: io.NopCloser(strings.NewReader("x"))}
	srv := stubServer(up, 64)
	defer srv.Close()

	if status, body := post(t, srv, bytes.Repeat([]byte("a"), 65), nil); status != 413 || !strings.HasPrefix(body, "body_limit ") {
		t.Fatalf("plain body over the cap: %d %q", status, body)
	}
	// A gzip bomb is small on the wire and large inflated.
	bomb := gz(t, strings.Repeat("a", 4096))
	if len(bomb) >= 64 {
		t.Fatalf("test bomb is %d bytes", len(bomb))
	}
	if status, body := post(t, srv, bomb, map[string]string{"Content-Encoding": "gzip"}); status != 413 || !strings.HasPrefix(body, "body_limit ") {
		t.Fatalf("inflated body over the cap: %d %q", status, body)
	}
	if status, body := post(t, srv, []byte("not gzip at all, but long enough"), map[string]string{"Content-Encoding": "gzip"}); status != 400 || !strings.HasPrefix(body, "body_invalid ") {
		t.Fatalf("bad gzip: %d %q", status, body)
	}
	truncated := gz(t, "0032want abc\n")
	if status, body := post(t, srv, truncated[:len(truncated)-6], map[string]string{"Content-Encoding": "gzip"}); status != 400 || !strings.HasPrefix(body, "body_invalid ") {
		t.Fatalf("truncated gzip: %d %q", status, body)
	}
	if status, _ := post(t, srv, []byte("0032want abc\n"), nil); status != 200 {
		t.Fatalf("a body under the cap: %d", status)
	}
}

func TestUpstreamFailuresAreOpaque(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{github.ErrNotInstalled, "repository_unavailable"},
		{github.ErrGitRefused, "upstream_refused"},
		{github.ErrGitTokenReflected, "upstream_invalid"},
		{errors.New("dial tcp 10.1.2.3:443: secret detail"), "upstream_unavailable"},
	} {
		up := &stubUpstream{err: tc.err}
		srv := stubServer(up, 0)
		status, body := post(t, srv, []byte("x"), nil)
		srv.Close()
		if status != 503 || !strings.HasPrefix(body, tc.code+" ") || strings.Contains(body, "secret") || strings.Contains(body, "10.1.2.3") {
			t.Fatalf("%v: %d %q", tc.err, status, body)
		}
	}
}

func TestLargeAnswerIsStreamed(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 64<<10) // 1 MiB
	up := &stubUpstream{reply: io.NopCloser(strings.NewReader(big))}
	srv := stubServer(up, 0)
	defer srv.Close()
	status, body := post(t, srv, []byte("x"), nil)
	if status != 200 || body != big {
		t.Fatalf("got %d, %d bytes", status, len(body))
	}
}

func TestProbes(t *testing.T) {
	srv := stubServer(&stubUpstream{}, 0)
	defer srv.Close()
	for _, path := range []string{"/health", "/ready"} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+path, http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	// Probes address the pod by IP, so they skip the Host check; nothing else does.
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/health", http.NoBody)
	req.Host = "10.1.2.3:8003"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe by IP: %d", resp.StatusCode)
	}
}

// syncBuffer is a log sink the server and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// failingBody yields one chunk, then fails.
type failingBody struct {
	sent bool
	err  error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if f.sent {
		return 0, f.err
	}
	f.sent = true
	return copy(p, "0008NAK\n"), nil
}

func (*failingBody) Close() error { return nil }

func TestUpstreamFailuresAreLoggedByClassNotText(t *testing.T) {
	cases := []struct {
		name  string
		up    *stubUpstream
		code  string
		class string
	}{
		{"before the status", &stubUpstream{err: fmt.Errorf("mint: %w", &net.OpError{Op: "dial", Err: errors.New("secret detail")})}, "upstream_unavailable", "*net.OpError"},
		{"unavailable", &stubUpstream{err: github.ErrUnavailable}, "upstream_unavailable", "github.ErrUnavailable"},
		{"cancelled", &stubUpstream{err: context.Canceled}, "upstream_unavailable", "context.Canceled"},
		{"refused", &stubUpstream{err: github.ErrGitRefused}, "upstream_refused", "github.ErrGitRefused"},
		{"truncated", &stubUpstream{err: io.ErrUnexpectedEOF}, "upstream_unavailable", "io.ErrUnexpectedEOF"},
		{"mid-stream", &stubUpstream{reply: &failingBody{err: github.ErrGitTokenReflected}}, "upstream_invalid", "github.ErrGitTokenReflected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			srv := &gittransport.Server{
				Auth:     fixedAuth{gittransport.Capability{WorkspaceID: "w", CredentialID: "c", Repository: "oldsj/alpha"}},
				Upstream: tc.up, Log: slog.New(slog.NewTextHandler(&logs, nil)), Dev: true,
			}
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/oldsj/alpha.git/git-upload-pack", strings.NewReader("x"))
			req.Header.Set("Authorization", "Bearer ml_good")
			req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			req.Header.Set("X-Request-Id", "req-123")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body) // a mid-stream abort surfaces here as an error
				_ = resp.Body.Close()
			}
			out := logs.String()
			if !strings.Contains(out, "code="+tc.code) || !strings.Contains(out, "request_id=") || !strings.Contains(out, "error_class=") || !strings.Contains(out, tc.class) {
				t.Fatalf("log lacks request id, code %s or class %s:\n%s", tc.code, tc.class, out)
			}
			if strings.Contains(out, "secret detail") {
				t.Fatalf("error text was logged:\n%s", out)
			}
		})
	}
}
