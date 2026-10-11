package kagent

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
)

// HITLExtension is sent on every request so kagent offers human-in-the-loop prompts.
const HITLExtension = "https://kagent.dev/extensions/hitl/v1"

const controlTokenMaxBytes = 4096

// tokenSource reads a bearer token file and re-reads it when its mtime or size changes. The
// token is held only in memory and never appears in an error.
type tokenSource struct {
	path string

	mu    sync.Mutex
	token string
	info  os.FileInfo // identity of the file the cached token came from
}

func (s *tokenSource) bearer() (string, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE must name a readable token file"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The cache key is the file itself (device and inode), its mtime and its size, so a replaced
	// file is re-read even if the new one has the same size and mtime.
	if s.info != nil && os.SameFile(s.info, info) && info.ModTime().Equal(s.info.ModTime()) && info.Size() == s.info.Size() {
		return s.token, nil
	}
	token, err := readToken(s.path)
	if err != nil {
		return "", err
	}
	s.token, s.info = token, info
	return token, nil
}

func readToken(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured path
	if err != nil {
		return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE must name a readable token file"}
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, controlTokenMaxBytes+1))
	if err != nil {
		return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE must name a readable token file"}
	}
	if len(raw) > controlTokenMaxBytes {
		return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE exceeds 4096 bytes"}
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if token == "" {
		return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE must contain one nonempty bearer"}
	}
	for i := range len(token) {
		if c := token[i]; c <= 32 || c >= 127 {
			return "", &ConfigError{Msg: "KAGENT_CONTROL_TOKEN_FILE must contain one nonempty ASCII bearer"}
		}
	}
	return token, nil
}

// authTransport adds the service identity and the HITL extension to every request, and tags a
// failure that happened before a connection existed, so callers can tell "never sent" from
// "maybe sent".
type authTransport struct {
	base   http.RoundTripper
	tokens *tokenSource // nil: send x-user-id
	userID string
	// host is the gateway's host:port. Credentials go to no other host.
	host string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	// The client follows no redirects, so every request is for the gateway. Even so, a request
	// for any other host gets no credentials.
	if req.URL.Host == t.host {
		if t.tokens != nil {
			token, err := t.tokens.bearer()
			if err != nil {
				if req.Body != nil {
					_ = req.Body.Close() // a RoundTripper must close the body even on failure
				}
				return nil, err
			}
			out.Header.Set("Authorization", "Bearer "+token)
		} else {
			out.Header.Set("x-user-id", t.userID)
		}
		out.Header.Set("A2A-Extensions", HITLExtension)
	}

	var connected bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected = true }}
	out = out.WithContext(httptrace.WithClientTrace(out.Context(), trace))

	resp, err := t.base.RoundTrip(out)
	if err != nil && !connected {
		return nil, &notSentError{err: err}
	}
	return resp, err
}

// doer adapts an *http.Client to connect's client interface and maps gateway-level failures:
// 401 and 403 become a ConfigError, and a 5xx, which says nothing about whether the call ran,
// becomes an outcome-unknown tag.
type doer struct{ client *http.Client }

func (d doer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req) //nolint:gosec // the URL is the operator-configured gateway
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, authRefused("HTTP " + strconv.Itoa(resp.StatusCode))
	case resp.StatusCode >= http.StatusInternalServerError:
		_ = resp.Body.Close()
		return nil, &statusError{status: resp.StatusCode}
	}
	return resp, nil
}
