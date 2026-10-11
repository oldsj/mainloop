package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

// DefaultGitBaseURL is the host git smart HTTP is spoken to.
const DefaultGitBaseURL = "https://github.com/"

// The two git services the read path may ask GitHub for, and their media types. Only upload-pack
// exists here: a push goes through the push gate, which owns its own upstream calls.
const (
	gitDiscoveryType = "application/x-git-upload-pack-advertisement"
	gitResultType    = "application/x-git-upload-pack-result"
	gitRequestType   = "application/x-git-upload-pack-request"
)

// gitIdleTimeout bounds how long a git response may go without a byte, and how long GitHub may
// take to start one.
const gitIdleTimeout = 60 * time.Second

// ErrGitRefused means GitHub answered a git request with something that is not the fetch
// protocol's answer: an error status, another media type, an encoding or a redirect. The answer's
// text is dropped, never relayed.
var ErrGitRefused = errors.New("github: git upstream refused the request")

// ErrGitTokenReflected means a git answer contained the installation token. The answer is cut off
// before the token reaches the caller.
var ErrGitTokenReflected = errors.New("github: git upstream response reflected the token")

// GitFetch is one upload-pack request to GitHub.
type GitFetch struct {
	// Discovery asks for the ref advertisement (GET info/refs); otherwise Body is POSTed to
	// git-upload-pack.
	Discovery bool
	// Protocol is the Git-Protocol header to forward: "", "version=1" or "version=2".
	Protocol string
	// Body is the request body of a POST. Its length is unknown to GitHub's side, so it is sent
	// chunked.
	Body io.Reader
}

// GitResponse is GitHub's answer to a [GitFetch]: a 200 with the exact git media type, streamed.
// Close it.
type GitResponse struct {
	ContentType string
	Body        io.ReadCloser
}

// GitFetch sends one upload-pack request for repo with a contents:read installation token and
// streams the answer. The token is minted here, scoped to the one repository, and is neither
// returned nor logged; a response that carries it is cut off. Redirects are not followed and any
// answer other than the fetch protocol's own is [ErrGitRefused].
func (a *App) GitFetch(ctx context.Context, repo Repo, f GitFetch) (*GitResponse, error) {
	if f.Protocol != "" && f.Protocol != "version=1" && f.Protocol != "version=2" {
		return nil, errors.New("github: unsupported git protocol version")
	}
	tr, err := a.gitTransport(ctx, repo)
	if err != nil {
		return nil, a.opaque(err)
	}
	tok, err := tr.Token(ctx)
	if err != nil {
		return nil, a.gitMintError(repo, err)
	}
	if tok == "" {
		return nil, &AuthError{}
	}
	u := a.gitBase.JoinPath(repo.Owner, repo.Name+".git")
	var (
		method = http.MethodPost
		body   io.Reader
		accept = gitResultType
	)
	if f.Discovery {
		method, accept = http.MethodGet, gitDiscoveryType
		u = u.JoinPath("info", "refs")
		u.RawQuery = "service=git-upload-pack"
	} else {
		u = u.JoinPath("git-upload-pack")
		body = f.Body
		if body == nil {
			body = bytes.NewReader(nil)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		cancel()
		return nil, ErrUnavailable
	}
	req.SetBasicAuth("x-access-token", tok)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "mainloop-git-transport")
	if !f.Discovery {
		req.Header.Set("Content-Type", gitRequestType)
	}
	if f.Protocol != "" {
		req.Header.Set("Git-Protocol", f.Protocol)
	}
	// No timer starts the request over: the watchdog below cancels it if GitHub goes quiet.
	dog := time.AfterFunc(gitIdleTimeout, cancel)
	resp, err := a.gitHTTP.Do(req)
	if err != nil {
		dog.Stop()
		cancel()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK || !gitHeadersOK(resp, accept, tok) {
		_ = resp.Body.Close()
		dog.Stop()
		cancel()
		return nil, ErrGitRefused
	}
	return &GitResponse{
		ContentType: accept,
		Body:        &gitBody{rc: resp.Body, dog: dog, token: []byte(tok), cancel: cancel},
	}, nil
}

// gitHeadersOK checks the answer is the plain, final, unframed-by-surprise one: the expected
// media type, no content encoding and no header that reflects the token.
func gitHeadersOK(resp *http.Response, want, token string) bool {
	if resp.Header.Get("Content-Type") != want || len(resp.Header.Values("Content-Type")) != 1 {
		return false
	}
	if enc := resp.Header.Values("Content-Encoding"); len(enc) > 1 || (len(enc) == 1 && !strings.EqualFold(enc[0], "identity")) {
		return false
	}
	if len(resp.Header.Values("Content-Length")) > 1 {
		return false
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	for _, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, token) || strings.Contains(v, basic) {
				return false
			}
		}
	}
	return true
}

// gitBody streams a response while resetting the idle watchdog on every read and refusing to
// pass on any byte that could be part of the token. It holds back the last len(token)-1 bytes
// until more data (or the end of the stream) shows they are not the start of the token, so a
// token split across reads never leaves even partly.
type gitBody struct {
	rc      io.ReadCloser
	dog     *time.Timer
	token   []byte
	cancel  context.CancelFunc
	pending []byte // scanned, held back
	ready   []byte // scanned, releasable
	done    error  // upstream's terminal error, delivered once ready drains
}

func (b *gitBody) Read(p []byte) (int, error) {
	buf := make([]byte, 32<<10)
	for len(b.ready) == 0 && b.done == nil {
		n, err := b.rc.Read(buf)
		b.dog.Reset(gitIdleTimeout)
		window := append(b.pending[:len(b.pending):len(b.pending)], buf[:n]...)
		if bytes.Contains(window, b.token) {
			return 0, ErrGitTokenReflected
		}
		keep := min(len(b.token)-1, len(window))
		if err != nil {
			keep, b.done = 0, err
		}
		b.ready = append(b.ready, window[:len(window)-keep]...)
		b.pending = append(b.pending[:0], window[len(window)-keep:]...)
	}
	if len(b.ready) > 0 {
		n := copy(p, b.ready)
		b.ready = b.ready[n:]
		return n, nil
	}
	return 0, b.done
}

func (b *gitBody) Close() error {
	b.dog.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// gitTransport returns the installation-token source for repo, scoped to contents:read, creating
// it (and finding the installation) on first use. It only mints: the token is attached to a git
// request by [App.GitFetch] and goes nowhere else.
func (a *App) gitTransport(ctx context.Context, repo Repo) (*ghinstallation.Transport, error) {
	scope := Scope{Repo: repo, Permissions: Permissions{"contents": Read}}
	key := "git|" + scopeKey(scope)
	a.mu.Lock()
	tr := a.gitTokens[key]
	a.mu.Unlock()
	if tr != nil {
		return tr, nil
	}
	v, err := a.once(ctx, key, func(ctx context.Context) (any, error) {
		a.mu.Lock()
		tr := a.gitTokens[key]
		a.mu.Unlock()
		if tr != nil {
			return tr, nil
		}
		fields, err := permissionFields(scope.Permissions)
		if err != nil {
			return nil, err
		}
		allow := a.allowlist(scope)
		id, err := a.installation(ctx, allow, scope.Repo)
		if err != nil {
			return nil, err
		}
		allow.BindInstallation(id)
		atr := ghinstallation.NewAppsTransportFromPrivateKey(allow, a.id, a.key)
		atr.BaseURL = strings.TrimSuffix(a.base.String(), "/")
		itr := ghinstallation.NewFromAppsTransport(atr, id)
		itr.BaseURL = atr.BaseURL
		itr.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{scope.Repo.Name}, Permissions: fields}
		a.mu.Lock()
		a.gitTokens[key] = itr
		a.mu.Unlock()
		return itr, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*ghinstallation.Transport), nil
}

// gitMintError reduces a failed mint to a typed error without upstream text, and forgets a
// vanished installation, as [tokenGuard] does for the API clients.
func (a *App) gitMintError(repo Repo, err error) error {
	var he *ghinstallation.HTTPError
	if !errors.As(err, &he) {
		if errors.Is(err, ErrInvalidToken) {
			return ErrInvalidToken
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || IsNotAllowed(err) {
			return a.opaque(err)
		}
		return &AuthError{}
	}
	status := 0
	if he.Response != nil {
		status = he.Response.StatusCode
		if he.Response.Body != nil {
			_ = he.Response.Body.Close()
		}
	}
	if status == http.StatusNotFound {
		a.forgetGit(repo)
		return ErrNotInstalled
	}
	return &AuthError{Status: status}
}

func (a *App) forgetGit(repo Repo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.gitTokens {
		if strings.HasPrefix(k, "git|"+strings.ToLower(repo.FullName())+"|") {
			delete(a.gitTokens, k)
		}
	}
	delete(a.installs, strings.ToLower(repo.FullName()))
}

// newGitClient builds the HTTP client git requests use: no redirects, no proxy (the token goes to
// the configured git host and nowhere else), and a bound on the wait for the first byte.
func newGitClient(base http.RoundTripper) *http.Client {
	rt := base
	if t, ok := base.(*http.Transport); ok {
		t = t.Clone()
		t.Proxy = nil
		t.ResponseHeaderTimeout = gitIdleTimeout
		rt = t
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func parseGitBase(raw string) (*url.URL, error) {
	if raw == "" {
		raw = DefaultGitBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("github: invalid git base URL")
	}
	return u, nil
}
