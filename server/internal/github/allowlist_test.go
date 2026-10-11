package github_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
)

type counting struct{ n atomic.Int32 }

func (c *counting) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func newAllowlist(t *testing.T, perms github.Permissions) (*github.Allowlist, *counting) {
	t.Helper()
	base, _ := url.Parse("https://api.github.com/")
	next := &counting{}
	return &github.Allowlist{
		Base:  base,
		Scope: github.Scope{Repo: github.Repo{Owner: "oldsj", Name: "mainloop"}, Permissions: perms},
		Next:  next,
	}, next
}

func do(t *testing.T, a *github.Allowlist, method, target, body string) error {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestAllowlistPassesTheKnownCalls(t *testing.T) {
	a, next := newAllowlist(t, github.Permissions{"metadata": github.Read, "pull_requests": github.Read})
	for _, c := range []struct{ method, url, body string }{
		{"GET", "https://api.github.com/repos/oldsj/mainloop", ""},
		{"GET", "https://api.github.com/repos/OldSJ/Mainloop", ""},
		{"GET", "https://api.github.com/repos/oldsj/mainloop/pulls?state=open&per_page=1", ""},
		{"GET", "https://api.github.com/repos/oldsj/mainloop/installation", ""},
		{"POST", "https://api.github.com/app/installations/77/access_tokens",
			`{"repositories":["mainloop"],"permissions":{"metadata":"read","pull_requests":"read"}}`},
	} {
		if err := do(t, a, c.method, c.url, c.body); err != nil {
			t.Errorf("%s %s: %v", c.method, c.url, err)
		}
	}
	if got := next.n.Load(); got != 5 {
		t.Fatalf("forwarded %d, want 5", got)
	}
}

// A refused request fails closed with a typed error and never reaches the next transport.
func TestAllowlistRefusesEverythingElseWithoutSending(t *testing.T) {
	a, next := newAllowlist(t, github.Permissions{"metadata": github.Read, "pull_requests": github.Read})
	const mint = "https://api.github.com/app/installations/77/access_tokens"
	for name, c := range map[string]struct{ method, url, body string }{
		"other host":          {"GET", "https://evil.example/repos/oldsj/mainloop", ""},
		"other scheme":        {"GET", "http://api.github.com/repos/oldsj/mainloop", ""},
		"userinfo":            {"GET", "https://x@api.github.com/repos/oldsj/mainloop", ""},
		"host lookalike":      {"GET", "https://api.github.com.evil.example/repos/oldsj/mainloop", ""},
		"other repo":          {"GET", "https://api.github.com/repos/oldsj/other", ""},
		"other owner":         {"GET", "https://api.github.com/repos/evil/mainloop", ""},
		"installation other":  {"GET", "https://api.github.com/repos/evil/mainloop/installation", ""},
		"write verb":          {"DELETE", "https://api.github.com/repos/oldsj/mainloop", ""},
		"patch repo":          {"PATCH", "https://api.github.com/repos/oldsj/mainloop", ""},
		"post pulls":          {"POST", "https://api.github.com/repos/oldsj/mainloop/pulls", "{}"},
		"merge":               {"PUT", "https://api.github.com/repos/oldsj/mainloop/pulls/1/merge", ""},
		"unlisted path":       {"GET", "https://api.github.com/user", ""},
		"unlisted repo path":  {"GET", "https://api.github.com/repos/oldsj/mainloop/issues", ""},
		"commits not granted": {"GET", "https://api.github.com/repos/oldsj/mainloop/commits", ""},
		"deeper path":         {"GET", "https://api.github.com/repos/oldsj/mainloop/pulls/1", ""},
		"app listing":         {"GET", "https://api.github.com/app/installations", ""},
		"app itself":          {"GET", "https://api.github.com/app", ""},
		"graphql":             {"POST", "https://api.github.com/graphql", "{}"},
		"encoded segment":     {"GET", "https://api.github.com/repos/oldsj/mainloop%2Fx", ""},
		"encoded dot":         {"GET", "https://api.github.com/repos/oldsj/%2e%2e/mainloop", ""},
		"dot segment":         {"GET", "https://api.github.com/repos/oldsj/mainloop/pulls/../../other", ""},
		"double slash":        {"GET", "https://api.github.com/repos//oldsj/mainloop", ""},
		"trailing slash":      {"GET", "https://api.github.com/repos/oldsj/mainloop/", ""},
		"backslash":           {"GET", "https://api.github.com/repos/oldsj/mainloop\\x", ""},
		"mint no body":        {"POST", mint, ""},
		"mint bad json":       {"POST", mint, "{"},
		"mint all repos":      {"POST", mint, `{"permissions":{"metadata":"read","pull_requests":"read"}}`},
		"mint other repo":     {"POST", mint, `{"repositories":["other"],"permissions":{"metadata":"read","pull_requests":"read"}}`},
		"mint two repos":      {"POST", mint, `{"repositories":["mainloop","other"],"permissions":{"metadata":"read","pull_requests":"read"}}`},
		"mint more perms":     {"POST", mint, `{"repositories":["mainloop"],"permissions":{"metadata":"read","pull_requests":"read","contents":"read"}}`},
		"mint fewer perms":    {"POST", mint, `{"repositories":["mainloop"],"permissions":{"metadata":"read"}}`},
		"mint write perm":     {"POST", mint, `{"repositories":["mainloop"],"permissions":{"metadata":"read","pull_requests":"write"}}`},
		"mint no perms":       {"POST", mint, `{"repositories":["mainloop"]}`},
		"mint extra field":    {"POST", mint, `{"repositories":["mainloop"],"repository_ids":[1],"permissions":{"metadata":"read","pull_requests":"read"}}`},
		"mint get":            {"GET", mint, ""},
	} {
		err := do(t, a, c.method, c.url, c.body)
		if !github.IsNotAllowed(err) {
			t.Errorf("%s: got %v, want a NotAllowedError", name, err)
		}
	}
	if got := next.n.Load(); got != 0 {
		t.Fatalf("%d refused requests reached the network", got)
	}
}

func TestAllowlistClosesRefusedBody(t *testing.T) {
	a, _ := newAllowlist(t, github.Permissions{"metadata": github.Read})
	body := &closeSpy{Reader: strings.NewReader("{}")}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://api.github.com/graphql", body)
	resp, err := a.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !github.IsNotAllowed(err) {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("body left open")
	}
}

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

func TestAllowlistWriteCoversRead(t *testing.T) {
	a, next := newAllowlist(t, github.Permissions{"pull_requests": github.Write})
	if err := do(t, a, "GET", "https://api.github.com/repos/oldsj/mainloop/pulls", ""); err != nil {
		t.Fatal(err)
	}
	if next.n.Load() != 1 {
		t.Fatal("not forwarded")
	}
}
