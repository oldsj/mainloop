package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Endpoints are the base URLs of the listeners, as Service DNS names in a cluster or as port-forwards
// from a laptop. They have no trailing slash.
type Endpoints struct {
	API     string
	MCP     string
	GitRead string
	GitPush string
}

const maxBody = 8 << 20

// caller sends requests with one deadline each. Non-2xx statuses are errors for the JSON calls and
// plain values for the probes, which need to tell 401 from a dead listener.
type caller struct {
	http    *http.Client
	host    string // Host header for owner API requests; empty keeps the URL's host
	timeout time.Duration
}

func (c caller) getJSON(ctx context.Context, target string, out any) error {
	return c.jsonCall(ctx, http.MethodGet, target, nil, out)
}

func (c caller) postJSON(ctx context.Context, target string, payload, out any) error {
	return c.jsonCall(ctx, http.MethodPost, target, payload, out)
}

func (c caller) jsonCall(ctx context.Context, method, target string, payload, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.host != "" {
		req.Host = c.host
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d", method, pathOf(target), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, pathOf(target), err)
	}
	return nil
}

// newCaller returns a caller that never follows redirects: each listener's own status is the answer.
func newCaller(host string, timeout time.Duration) caller {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return caller{http: client, host: host, timeout: timeout}
}

// probe sends one request, with host as its Host header when set, and returns the status and the first 256 bytes of the body.
func (c caller) probe(ctx context.Context, method, target, host string, header http.Header) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if host != "" {
		req.Host = host
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return resp.StatusCode, string(head), nil
}

// pathOf keeps a URL's path for error text and drops its query, which can carry a branch name.
func pathOf(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "request"
	}
	return u.Path
}
