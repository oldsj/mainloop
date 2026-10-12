package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
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
// "{owner}" or "{repo}" (bound to the scoped repository), or "*" (any one segment). A final "**"
// takes one or more segments, for branch names that contain a slash.
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
	// Pull requests and merges (pulls.go). The merge call is the only write that changes a branch.
	newRule(http.MethodGet, "/repos/{owner}/{repo}/pulls/*", kindRepo, Permissions{"pull_requests": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/pulls/*/files", kindRepo, Permissions{"pull_requests": Read}),
	newRule(http.MethodPost, "/repos/{owner}/{repo}/pulls", kindRepo, Permissions{"pull_requests": Write}),
	newRule(http.MethodPut, "/repos/{owner}/{repo}/pulls/*/merge", kindRepo,
		Permissions{"contents": Write, "pull_requests": Write}),
	// Protection must precede the general branch rule so it asks for administration.
	newRule(http.MethodGet, "/repos/{owner}/{repo}/branches/*/protection", kindRepo, Permissions{"administration": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/branches/**", kindRepo, Permissions{"contents": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/rules/branches/*", kindRepo, Permissions{"metadata": Read}),
	// Commit comparison, for the merge inventory: base and head are 40-hex SHAs joined by "...".
	newRule(http.MethodGet, "/repos/{owner}/{repo}/compare/*", kindRepo, Permissions{"contents": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/commits/*/check-suites", kindRepo, Permissions{"checks": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/commits/*/check-runs", kindRepo, Permissions{"checks": Read}),
	newRule(http.MethodGet, "/repos/{owner}/{repo}/commits/*/statuses", kindRepo, Permissions{"statuses": Read}),
}

// Scope is what one client may touch: a single repository and the permissions of its token.
type Scope struct {
	Repo        Repo
	Permissions Permissions
}

// Allowlist is the RoundTripper every GitHub request passes through. It forwards a request to
// Next only if its origin is the configured API, its method and path match a rule, and — for
// repository calls — the path names the scoped repository and the scope's permissions cover the
// rule. A token-minting call must name the bound installation and ask for exactly the scoped
// repository and permissions, and its answer must carry a usable token. Anything else fails
// with a [*NotAllowedError] (or [ErrInvalidToken]) and never reaches Next.
type Allowlist struct {
	// Base is the API origin (and optional path prefix), such as https://api.github.com/.
	Base  *url.URL
	Scope Scope
	Next  http.RoundTripper
	// Now is the clock for judging a token's expiry; nil means time.Now.
	Now func() time.Time

	// installation is the id the App's installation on Scope.Repo resolved to. Until it is bound,
	// every mint is refused.
	installation atomic.Int64
}

// BindInstallation records the installation id found for the scoped repository. Token requests
// for any other installation are refused.
func (a *Allowlist) BindInstallation(id int64) { a.installation.Store(id) }

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
	if !isMint(r) {
		return a.Next.RoundTrip(req)
	}
	// checkMint consumes and closes the original body itself.
	if req, err = a.checkMint(req); err != nil {
		return nil, err
	}
	resp, err := a.Next.RoundTrip(req)
	if err != nil || resp.StatusCode/100 != 2 {
		return resp, err
	}
	return a.checkToken(resp)
}

func isMint(r rule) bool { return r.kind == kindApp && r.method == http.MethodPost }

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
		if isMint(r) {
			id := a.installation.Load()
			if id <= 0 || segs[len(segs)-2] != strconv.FormatInt(id, 10) {
				return rule{}, a.deny(req, "outside the repository's installation")
			}
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
	if n := len(pattern); pattern[n-1] == "**" {
		if len(segs) < n {
			return "", "", false
		}
		pattern, segs = pattern[:n-1], segs[:n-1]
	} else if n != len(segs) {
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
	Repositories []string
	Permissions  map[string]string
}

const maxMintBody = 4 << 10

var errMintBody = errors.New("malformed token request")

// decodeMint parses a token request strictly: one JSON object, no unknown or repeated keys (also
// among the permissions), and nothing after it. Go's decoder alone would take the last of two
// repeated keys and ignore trailing values, so the check could disagree with the receiver.
func decodeMint(raw []byte) (mintBody, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out mintBody
	seen := map[string]bool{}
	err := walkObject(dec, func(key string) error {
		if seen[key] {
			return errMintBody
		}
		seen[key] = true
		switch key {
		case "repositories":
			return dec.Decode(&out.Repositories)
		case "permissions":
			out.Permissions = map[string]string{}
			return walkObject(dec, func(name string) error {
				if _, dup := out.Permissions[name]; dup {
					return errMintBody
				}
				var level string
				if err := dec.Decode(&level); err != nil {
					return err
				}
				out.Permissions[name] = level
				return nil
			})
		default:
			return errMintBody
		}
	})
	if err != nil {
		return mintBody{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return mintBody{}, errMintBody
	}
	return out, nil
}

// walkObject reads one JSON object, calling each with every key; each must consume the value.
func walkObject(dec *json.Decoder, each func(key string) error) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errMintBody
	}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return errMintBody
		}
		if err := each(key); err != nil {
			return err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errMintBody
	}
	return nil
}

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
	body, err := decodeMint(raw)
	if err != nil || len(body.Repositories) != 1 ||
		body.Repositories[0] != a.Scope.Repo.Name || !Permissions(body.Permissions).equal(a.Scope.Permissions) {
		return nil, a.deny(req, "token request is not scoped to the client's repository and permissions")
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(raw))
	out.ContentLength = int64(len(raw))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	return out, nil
}

// ErrInvalidToken means GitHub answered a token request with something unusable: no token, a token
// with whitespace in it, or an expiry that is missing or already inside the refresh margin. The
// answer is never passed on, so nothing caches it.
var ErrInvalidToken = errors.New("github: installation token response invalid")

const (
	// refreshMargin is how long before expiry the token transport mints a new token.
	refreshMargin = time.Minute
	maxTokenBody  = 64 << 10
)

// checkToken validates a successful token answer and hands back a response whose body can be read again.
func (a *Allowlist) checkToken(resp *http.Response) (*http.Response, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, a.opaqueRead(err)
	}
	var got struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if len(raw) > maxTokenBody || json.Unmarshal(raw, &got) != nil || got.Token == "" ||
		strings.IndexFunc(got.Token, unicode.IsSpace) >= 0 || !got.ExpiresAt.After(now().Add(refreshMargin)) {
		return nil, ErrInvalidToken
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

// opaqueRead keeps the size-cap and context errors typed and turns any other read failure into ErrUnavailable.
func (a *Allowlist) opaqueRead(err error) error {
	switch {
	case errors.Is(err, ErrResponseTooLarge), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return ErrUnavailable
	}
}

// IsNotAllowed reports whether err is an allowlist refusal.
func IsNotAllowed(err error) bool {
	var na *NotAllowedError
	return errors.As(err, &na)
}
