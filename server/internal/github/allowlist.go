package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Permission levels GitHub accepts for an installation token.
const (
	Read  = "read"
	Write = "write"
)

// Permissions maps a GitHub App permission name to "read" or "write".
type Permissions map[string]string

// covers reports whether p grants everything need asks for. Write covers read.
func (p Permissions) covers(need Permissions) bool {
	for name, level := range need {
		have, ok := p[name]
		if !ok || (have != level && have != Write) {
			return false
		}
	}
	return true
}

func (p Permissions) equal(o Permissions) bool {
	if len(p) != len(o) {
		return false
	}
	for name, level := range p {
		if o[name] != level {
			return false
		}
	}
	return true
}

// NotAllowedError is returned, without sending anything, for a request the allowlist refuses.
type NotAllowedError struct {
	Method string
	Path   string
	Reason string
}

func (e *NotAllowedError) Error() string {
	return fmt.Sprintf("github: %s %s not allowed: %s", e.Method, e.Path, e.Reason)
}

// kind says how a rule's request authenticates.
type kind int

const (
	// kindRepo calls are made with the repository-scoped installation token.
	kindRepo kind = iota
	// kindApp calls are made with the App's own JWT, only to find the installation and mint tokens.
	kindApp
)

// rule is one allowed endpoint: a method and a path pattern. A pattern segment is literal,
// "{owner}" or "{repo}" (bound to the scoped repository), or "*" (any one segment).
type rule struct {
	method  string
	pattern []string
	kind    kind
	// needs are the installation permissions the call requires; the scoped token must cover them.
	needs Permissions
}

func newRule(method, pattern string, k kind, needs Permissions) rule {
	return rule{method: method, pattern: strings.Split(strings.Trim(pattern, "/"), "/"), kind: k, needs: needs}
}

// rules is the whole of what Mainloop may ask GitHub. Add an entry here, with the least
// permission that serves it, before using a new endpoint.
var rules = []rule{
	newRule(http.MethodGet, "/repos/{owner}/{repo}/installation", kindApp, nil),
	newRule(http.MethodPost, "/app/installations/*/access_tokens", kindApp, nil),
	newRule(http.MethodGet, "/repos/{owner}/{repo}", kindRepo, Permissions{"metadata": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/pulls", kindRepo, Permissions{"pull_requests": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/commits", kindRepo, Permissions{"contents": Read}),
}

// Scope is what one client may touch: a single repository and the permissions of its token.
type Scope struct {
	Repo        Repo
	Permissions Permissions
}

// Allowlist is the RoundTripper every GitHub request passes through. It forwards a request to
// Next only if its origin is the configured API, its method and path match a rule, and — for
// repository calls — the path names the scoped repository and the scope's permissions cover the
// rule. A token-minting call must ask for exactly the scoped repository and permissions. Anything
// else fails with a [*NotAllowedError] and never reaches Next.
type Allowlist struct {
	// Base is the API origin (and optional path prefix), such as https://api.github.com/.
	Base  *url.URL
	Scope Scope
	Next  http.RoundTripper
}

// RoundTrip implements [http.RoundTripper].
func (a *Allowlist) RoundTrip(req *http.Request) (*http.Response, error) {
	closeBody := func() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
	}
	r, err := a.match(req)
	if err != nil {
		closeBody()
		return nil, err
	}
	if r.kind == kindApp && r.method == http.MethodPost {
		// checkMint consumes and closes the original body itself.
		if req, err = a.checkMint(req); err != nil {
			return nil, err
		}
	}
	return a.Next.RoundTrip(req)
}

func (a *Allowlist) deny(req *http.Request, reason string) error {
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	return &NotAllowedError{Method: req.Method, Path: path, Reason: reason}
}

func (a *Allowlist) match(req *http.Request) (rule, error) {
	u := req.URL
	if u == nil || u.Scheme != a.Base.Scheme || !strings.EqualFold(u.Host, a.Base.Host) {
		return rule{}, a.deny(req, "other origin")
	}
	if u.User != nil || u.Opaque != "" || u.Fragment != "" || (req.Host != "" && !strings.EqualFold(req.Host, a.Base.Host)) {
		return rule{}, a.deny(req, "malformed request")
	}
	prefix := strings.TrimSuffix(a.Base.Path, "/")
	path := u.EscapedPath()
	if !strings.HasPrefix(path, prefix+"/") {
		return rule{}, a.deny(req, "outside the API root")
	}
	rest := strings.TrimPrefix(path, prefix)
	// No escapes, backslashes or dot or empty segments: the path means what it says.
	if strings.ContainsAny(rest, "%\\") || strings.Contains(rest, "//") {
		return rule{}, a.deny(req, "unclean path")
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if strings.HasSuffix(rest, "/") {
		return rule{}, a.deny(req, "unclean path")
	}
	for _, s := range segs {
		if s == "." || s == ".." {
			return rule{}, a.deny(req, "unclean path")
		}
	}
	for _, r := range rules {
		if r.method != req.Method {
			continue
		}
		owner, name, ok := bind(r.pattern, segs)
		if !ok {
			continue
		}
		if r.kind == kindRepo || owner != "" {
			if !strings.EqualFold(owner, a.Scope.Repo.Owner) || !strings.EqualFold(name, a.Scope.Repo.Name) {
				return rule{}, a.deny(req, "outside the token's repository")
			}
		}
		if r.kind == kindRepo && !a.Scope.Permissions.covers(r.needs) {
			return rule{}, a.deny(req, "outside the token's permissions")
		}
		return r, nil
	}
	return rule{}, a.deny(req, "no matching rule")
}

// bind matches segs against pattern. It returns the bound owner and repo, empty when the
// pattern has none.
func bind(pattern, segs []string) (owner, name string, ok bool) {
	if len(pattern) != len(segs) {
		return "", "", false
	}
	for i, p := range pattern {
		switch p {
		case "{owner}":
			owner = segs[i]
		case "{repo}":
			name = segs[i]
		case "*":
		default:
			if p != segs[i] {
				return "", "", false
			}
		}
	}
	return owner, name, true
}

// mintBody is the JSON body of POST /app/installations/{id}/access_tokens.
type mintBody struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

const maxMintBody = 4 << 10

// checkMint reads the token request and refuses it unless it names exactly the scoped
// repository and permissions. The body is restored for sending.
func (a *Allowlist) checkMint(req *http.Request) (*http.Request, error) {
	if req.Body == nil {
		return nil, a.deny(req, "token request must be scoped")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxMintBody+1))
	_ = req.Body.Close()
	if err != nil || len(raw) > maxMintBody {
		return nil, a.deny(req, "token request unreadable")
	}
	var body mintBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || len(body.Repositories) != 1 ||
		body.Repositories[0] != a.Scope.Repo.Name || !Permissions(body.Permissions).equal(a.Scope.Permissions) {
		return nil, a.deny(req, "token request is not scoped to the client's repository and permissions")
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(raw))
	out.ContentLength = int64(len(raw))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	return out, nil
}

// IsNotAllowed reports whether err is an allowlist refusal.
func IsNotAllowed(err error) bool {
	var na *NotAllowedError
	return errors.As(err, &na)
}
