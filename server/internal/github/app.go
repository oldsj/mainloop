package github

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com/"

// Typed failures. None carries upstream text, so a caller can log or return them freely.
var (
	// ErrNotInstalled means the GitHub App is not installed on the repository.
	ErrNotInstalled = errors.New("github: App is not installed on the repository")
	// ErrNotFound means GitHub has no such repository or resource, or the App cannot see it.
	ErrNotFound = errors.New("github: not found")
)

// StatusError is any other non-2xx answer. It holds the status only.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("github: upstream status %d", e.Status) }

// AuthError means an installation token could not be obtained.
type AuthError struct{ Status int }

func (e *AuthError) Error() string {
	if e.Status == 0 {
		return "github: installation token unavailable"
	}
	return fmt.Sprintf("github: installation token refused with status %d", e.Status)
}

// Options configure [NewApp].
type Options struct {
	// AppID and PrivateKey are the GitHub App credentials; PrivateKey is a single-line base64 PEM.
	AppID      string
	PrivateKey string
	// BaseURL is the API root. Empty means [DefaultBaseURL]; tests point it at githubtest.
	BaseURL string
	// Transport carries the requests once the allowlist has passed them. Nil means a clone of
	// the default transport.
	Transport http.RoundTripper
}

// App authenticates as a GitHub App and hands out clients scoped to one repository and the
// least permissions a use needs. Installation tokens are minted on demand, cached by the
// ghinstallation transport until a minute before they expire, and never logged or returned.
type App struct {
	id   int64
	key  *rsa.PrivateKey
	base *url.URL
	net  http.RoundTripper

	mu       sync.Mutex
	installs map[string]int64
	clients  map[string]*gh.Client
}

// NewApp validates the credentials. Errors never include the key.
func NewApp(o Options) (*App, error) {
	id, err := ParseAppID(o.AppID)
	if err != nil {
		return nil, err
	}
	key, err := ParsePrivateKey(o.PrivateKey)
	if err != nil {
		return nil, err
	}
	raw := o.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return nil, errors.New("github: invalid API base URL")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	net := o.Transport
	if net == nil {
		net = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &App{
		id: id, key: key, base: base, net: net,
		installs: map[string]int64{},
		clients:  map[string]*gh.Client{},
	}, nil
}

// permissionFields sets the one InstallationPermissions field a permission name stands for.
func permissionFields(p Permissions) (*gh.InstallationPermissions, error) {
	out := &gh.InstallationPermissions{}
	for name, level := range p {
		if level != Read && level != Write {
			return nil, fmt.Errorf("github: unknown permission level %q", level)
		}
		l := level
		switch name {
		case "metadata":
			out.Metadata = &l
		case "contents":
			out.Contents = &l
		case "pull_requests":
			out.PullRequests = &l
		case "issues":
			out.Issues = &l
		case "checks":
			out.Checks = &l
		case "statuses":
			out.Statuses = &l
		default:
			return nil, fmt.Errorf("github: unknown permission %q", name)
		}
	}
	return out, nil
}

func scopeKey(s Scope) string {
	names := make([]string, 0, len(s.Permissions))
	for n, l := range s.Permissions {
		names = append(names, n+"="+l)
	}
	sort.Strings(names)
	return strings.ToLower(s.Repo.FullName()) + "|" + strings.Join(names, ",")
}

func (a *App) newHTTP(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: rt,
		// A redirect is an answer, not an instruction: the allowlist never sees its target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (a *App) newGitHub(rt http.RoundTripper) (*gh.Client, error) {
	base := a.base.String()
	return gh.NewClient(gh.WithHTTPClient(a.newHTTP(rt)), gh.WithURLs(&base, &base))
}

func (a *App) allowlist(s Scope) *Allowlist {
	return &Allowlist{
		Base:  a.base,
		Scope: s,
		Next:  bounded{next: a.net, timeout: requestTimeout, max: maxResponse},
	}
}

// client returns the cached client for a scope, creating it (and finding the installation) on
// first use. The returned client carries a token scoped to repo with exactly perms.
func (a *App) client(ctx context.Context, repo Repo, perms Permissions) (*gh.Client, error) {
	scope := Scope{Repo: repo, Permissions: perms}
	key := scopeKey(scope)
	a.mu.Lock()
	c := a.clients[key]
	a.mu.Unlock()
	if c != nil {
		return c, nil
	}
	fields, err := permissionFields(perms)
	if err != nil {
		return nil, err
	}
	allow := a.allowlist(scope)
	id, err := a.installation(ctx, allow, repo)
	if err != nil {
		return nil, err
	}
	// One AppsTransport per token transport: ghinstallation mutates it while refreshing.
	atr := ghinstallation.NewAppsTransportFromPrivateKey(allow, a.appID(), a.key)
	atr.BaseURL = strings.TrimSuffix(a.base.String(), "/")
	itr := ghinstallation.NewFromAppsTransport(atr, id)
	itr.BaseURL = atr.BaseURL
	itr.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{repo.Name}, Permissions: fields}
	c, err = a.newGitHub(&tokenGuard{app: a, key: scopeKey(scope), repo: repo, next: itr})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	// A concurrent caller may have built the same client; keep the first so its token is shared.
	if prev := a.clients[key]; prev != nil {
		c = prev
	} else {
		a.clients[key] = c
	}
	a.mu.Unlock()
	return c, nil
}

func (a *App) appID() int64 { return a.id }

// installation finds the App's installation on repo, caching the id.
func (a *App) installation(ctx context.Context, allow *Allowlist, repo Repo) (int64, error) {
	k := strings.ToLower(repo.FullName())
	a.mu.Lock()
	id, ok := a.installs[k]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	atr := ghinstallation.NewAppsTransportFromPrivateKey(allow, a.id, a.key)
	atr.BaseURL = strings.TrimSuffix(a.base.String(), "/")
	app, err := a.newGitHub(atr)
	if err != nil {
		return 0, err
	}
	inst, _, err := app.Apps.GetRepositoryInstallation(ctx, repo.Owner, repo.Name)
	if err != nil {
		var er *gh.ErrorResponse
		if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound {
			return 0, ErrNotInstalled
		}
		return 0, a.opaque(err)
	}
	if inst.GetID() <= 0 {
		return 0, &AuthError{}
	}
	a.mu.Lock()
	a.installs[k] = inst.GetID()
	a.mu.Unlock()
	return inst.GetID(), nil
}

// forget drops what was learned about a repository's installation, so the next use looks again.
func (a *App) forget(key string, repo Repo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.clients, key)
	delete(a.installs, strings.ToLower(repo.FullName()))
}

// ErrUnavailable means GitHub could not be reached or answered nonsense.
var ErrUnavailable = errors.New("github: unavailable")

// opaque reduces an error to one that holds no upstream text.
func (a *App) opaque(err error) error {
	var (
		na *NotAllowedError
		ae *AuthError
		se *StatusError
		er *gh.ErrorResponse
		rl *gh.RateLimitError
		ab *gh.AbuseRateLimitError
	)
	switch {
	case errors.As(err, &na):
		return na
	case errors.Is(err, ErrNotInstalled):
		return ErrNotInstalled
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &se):
		return se
	case errors.Is(err, ErrResponseTooLarge):
		return ErrResponseTooLarge
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.As(err, &rl) && rl.Response != nil:
		return &StatusError{Status: rl.Response.StatusCode}
	case errors.As(err, &ab) && ab.Response != nil:
		return &StatusError{Status: ab.Response.StatusCode}
	case errors.As(err, &er) && er.Response != nil:
		if er.Response.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return &StatusError{Status: er.Response.StatusCode}
	default:
		return ErrUnavailable
	}
}

// tokenGuard wraps the installation-token transport so a failed mint becomes a typed error
// without upstream text, and a vanished installation is forgotten.
type tokenGuard struct {
	app  *App
	key  string
	repo Repo
	next http.RoundTripper
}

func (g *tokenGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	var na *NotAllowedError
	if errors.As(err, &na) {
		return nil, na
	}
	var he *ghinstallation.HTTPError
	if errors.As(err, &he) {
		status := 0
		if he.Response != nil {
			status = he.Response.StatusCode
			if he.Response.Body != nil {
				_ = he.Response.Body.Close()
			}
		}
		if status == http.StatusNotFound {
			g.app.forget(g.key, g.repo)
			return nil, ErrNotInstalled
		}
		if status != 0 {
			return nil, &AuthError{Status: status}
		}
	}
	if errors.Is(err, ErrResponseTooLarge) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, g.app.opaque(err)
	}
	return nil, &AuthError{}
}

// String and GoString keep the key and tokens out of any log line or %v that meets an App.
func (a *App) String() string   { return fmt.Sprintf("github.App(%d)", a.id) }
func (a *App) GoString() string { return a.String() }
