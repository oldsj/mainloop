// Package githubtest is an in-process fake of the slice of GitHub that Mainloop uses: finding an
// App installation, minting repository-scoped installation tokens, and reading a repository, its
// open pull requests and its commits.
//
// The fake checks what real GitHub checks — the App's RS256 JWT on App calls, and a live
// installation token whose repository and permissions cover the call on everything else — so a
// test of the client fails here if the client asks for the wrong scope. It records every request
// it receives, so a test can prove that a refused call never arrived.
package githubtest

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// AppID is the fake App's id.
const AppID = 4242

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
	keyB64  string
)

func loadKey() {
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			panic(err)
		}
		key = k
		keyB64 = base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
}

// PrivateKey returns the fake App's private key as the single-line base64 PEM the product reads
// from its environment.
func PrivateKey() string {
	loadKey()
	return keyB64
}

// PullRequest is an open pull request in the fake.
type PullRequest struct {
	Number    int
	Title     string
	Author    string
	HeadRef   string
	CreatedAt time.Time
	UpdatedAt time.Time

	// The fields below serve the pull-request and merge calls (pulls.go). The zero values make an
	// open, mergeable, non-draft pull request into the default branch.

	Body string
	// Closed marks a closed pull request; Merged also sets it.
	Closed bool
	Merged bool
	Draft  bool
	// HeadSHA is the head commit; BaseRef is the base branch, the default branch when empty.
	HeadSHA string
	BaseRef string
	// MergeCommitSHA is set when the pull request is merged.
	MergeCommitSHA string
	// Unmergeable answers mergeable false; MergeableUnknown answers null; MergeableState defaults
	// to "clean".
	Unmergeable      bool
	MergeableUnknown bool
	MergeableState   string
	Files            []File
	// Additions, Deletions and ChangedFiles override the totals computed from Files when nonzero.
	Additions    int
	Deletions    int
	ChangedFiles int
	// HeadRepoGone answers a null head repository (a deleted fork).
	HeadRepoGone bool
}

// Commit is a commit in the fake. Commits are served newest first, in the order given.
type Commit struct {
	SHA     string
	Message string
	Author  string
	Date    time.Time
}

// Repository is a repository in the fake. Issues counts open issues that are not pull requests;
// GitHub's own open_issues_count adds the pull requests, and so does the fake.
type Repository struct {
	Owner         string
	Name          string
	Description   *string
	DefaultBranch string
	AvatarURL     string
	Issues        int
	PullRequests  []PullRequest
	Commits       []Commit
	// NotInstalled makes the App unknown to this repository (404 on the installation lookup).
	NotInstalled bool

	// The fields below serve the pull-request and merge calls (pulls.go).

	// ID is the repository id; zero means 1.
	ID int64
	// NoSquash turns squash merging off; Archived marks the repository archived.
	NoSquash bool
	Archived bool
	// Branches maps a branch name to the commit it points at. The default branch is added with a
	// fixed tip when absent.
	Branches map[string]string
	// Suites, Runs and Statuses are the CI facts, by commit.
	Suites   map[string][]CheckSuite
	Runs     map[string][]CheckRun
	Statuses map[string][]Status
	// Protection is classic branch protection of the default branch; nil means none.
	// ProtectionStatus, when set, is the status the protection read answers instead, with
	// ProtectionMessage as its message (default: an upstream-looking secret marker).
	// ProtectionRaw, when set, is the JSON the read answers instead of Protection.
	Protection        *Protection
	ProtectionStatus  int
	ProtectionMessage string
	ProtectionRaw     string
	// Rules are the active ruleset rules; RulesStatus, when set, is the status that read answers
	// (RulesMessage its message; RulesLaterStatus the status for pages after the first only).
	// RulesRaw, when set, is the JSON array the read answers instead of Rules.
	Rules            []Rule
	RulesStatus      int
	RulesMessage     string
	RulesLaterStatus int
	RulesRaw         string
	// OmitSquash leaves allow_squash_merge out of the repository answer, as GitHub does for a
	// token that has no Contents access (the fake also leaves it out for such a token).
	OmitSquash bool
	// Compares holds the answers to commit comparisons, keyed "base...head". A comparison of the
	// default branch tip with the head of a pull request that has none is answered as a head one
	// commit ahead of the base, listing the pull request's files.
	Compares map[string]Compare
	// MergeStatus, when set, is the status a merge request answers instead of merging.
	MergeStatus int
	// IssueList are the repository's issues, read by number (issues.go in the parent package).
	IssueList []Issue
	// DependabotAlerts are the repository's open Dependabot alerts.
	DependabotAlerts []DependabotAlert
}

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Query  string
	// Body is the request body, for the calls that send one (a pull request, a merge).
	Body string
}

// Mint is one installation-token request the fake served.
type Mint struct {
	InstallationID int64
	Repositories   []string
	Permissions    map[string]string
}

type token struct {
	repo    string
	perms   map[string]string
	expires time.Time
}

// Server is the fake. Create it with [New].
type Server struct {
	// URL is the API root, with a trailing slash. Hand it to the client as its base URL.
	URL string

	// TokenTTL is how long a minted token lives. Zero means an hour. A TTL under a minute makes
	// every token already due for refresh, which forces a fresh mint on each use.
	TokenTTL time.Duration

	// MintResponse, when set, builds the body of every successful token answer from the token
	// and expiry the fake would have used, so a test can serve a malformed one.
	MintResponse func(token string, expires time.Time) map[string]any

	srv *httptest.Server

	mu        sync.Mutex
	repos     map[string]*Repository
	requests  []Request
	mints     []Mint
	tokens    map[string]token
	failures  map[string]int
	redirects map[string]string
	// rateLimited and lost are keyed like failures; lost counts replies still to be dropped.
	rateLimited map[string]bool
	lost        map[string]int
	// before holds the functions to run, once each, before the next request for method and path.
	before map[string][]func()
	nextPR map[string]int
}

// New starts a fake and closes it when t finishes.
func New(t testing.TB) *Server {
	t.Helper()
	loadKey()
	s := &Server{
		repos: map[string]*Repository{}, tokens: map[string]token{}, failures: map[string]int{}, redirects: map[string]string{},
		rateLimited: map[string]bool{}, lost: map[string]int{}, before: map[string][]func(){}, nextPR: map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + "/"
	t.Cleanup(s.srv.Close)
	return s
}

func lower(owner, name string) string { return strings.ToLower(owner + "/" + name) }

// AddRepo adds or replaces a repository.
func (s *Server) AddRepo(r Repository) {
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[lower(r.Owner, r.Name)] = &r
}

// Fail makes every request for method and path (for example "GET", "/repos/a/b") answer with
// status and an upstream-looking body. Status 0 removes the failure.
func (s *Server) Fail(method, path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status == 0 {
		delete(s.failures, method+" "+path)
		return
	}
	s.failures[method+" "+path] = status
}

// Before runs fn, once, when the next request for method and path arrives and before it is answered, so
// a test can change the world in the middle of a read. Several hooks for one request run in order.
func (s *Server) Before(method, path string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before[method+" "+path] = append(s.before[method+" "+path], fn)
}

// Redirect makes GET path answer 302 to target, so a test can prove the client does not follow it.
func (s *Server) Redirect(path, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirects[path] = target
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Mints returns every installation-token request served, in order.
func (s *Server) Mints() []Mint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mint(nil), s.mints...)
}

// SecretMarker is text the fake puts in error bodies, so a test can check that the client keeps
// upstream text out of its errors and logs.
const SecretMarker = "upstream-secret-marker"

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	key := r.Method + " " + r.URL.Path
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
	status := s.failures[key]
	limited := s.rateLimited[key]
	target := s.redirects[r.URL.Path]
	lose := s.lost[key] > 0
	hooks := s.before[key]
	delete(s.before, key)
	if lose {
		s.lost[key]--
	}
	s.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
	if target != "" {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(2*time.Second).Unix(), 10))
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	if status != 0 {
		writeJSON(w, status, map[string]string{"message": SecretMarker})
		return
	}
	if lose {
		// The request is carried out, and the connection is dropped before the answer.
		s.dispatch(httptest.NewRecorder(), r)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	s.dispatch(w, r)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(segs) == 4 && segs[0] == "repos" && segs[3] == "installation":
		s.installation(w, r, segs[1], segs[2])
	case r.Method == http.MethodPost && len(segs) == 4 && segs[0] == "app" && segs[1] == "installations" && segs[3] == "access_tokens":
		s.mint(w, r, segs[2])
	case len(segs) >= 3 && segs[0] == "repos":
		s.repoCall(w, r, segs)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) appJWT(w http.ResponseWriter, r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return false
	}
	claims := &jwt.RegisteredClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !tok.Valid || claims.Issuer != strconv.Itoa(AppID) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return false
	}
	return true
}

func installationID(owner, name string) int64 {
	var sum int64
	for _, c := range lower(owner, name) {
		sum = sum*31 + int64(c)
		sum %= 1_000_000
	}
	return sum + 1
}

func (s *Server) installation(w http.ResponseWriter, r *http.Request, owner, name string) {
	if !s.appJWT(w, r) {
		return
	}
	s.mu.Lock()
	repo := s.repos[lower(owner, name)]
	s.mu.Unlock()
	if repo == nil || repo.NotInstalled {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": installationID(owner, name), "app_id": AppID})
}

func (s *Server) mint(w http.ResponseWriter, r *http.Request, id string) {
	if !s.appJWT(w, r) {
		return
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Repositories) != 1 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	s.mu.Lock()
	var found *Repository
	for _, repo := range s.repos {
		if strings.EqualFold(repo.Name, body.Repositories[0]) && installationID(repo.Owner, repo.Name) == n && !repo.NotInstalled {
			found = repo
		}
	}
	if found == nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	ttl := s.TokenTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	value := fmt.Sprintf("ghs_fake_%d", len(s.mints)+1)
	expires := time.Now().Add(ttl).UTC()
	s.tokens[value] = token{repo: lower(found.Owner, found.Name), perms: body.Permissions, expires: expires}
	s.mints = append(s.mints, Mint{InstallationID: n, Repositories: body.Repositories, Permissions: body.Permissions})
	s.mu.Unlock()
	answer := map[string]any{
		"token": value, "expires_at": expires.Format(time.RFC3339),
		"permissions": body.Permissions,
	}
	if s.MintResponse != nil {
		answer = s.MintResponse(value, expires)
	}
	writeJSON(w, http.StatusCreated, answer)
}

// need is the permissions an endpoint requires, by name and level. Nil means no such endpoint.
func need(method string, segs []string) map[string]string {
	rest := segs[3:]
	read := func(name string) map[string]string { return map[string]string{name: "read"} }
	switch {
	case method == http.MethodGet && len(rest) == 0:
		return read("metadata")
	case method == http.MethodGet && len(rest) == 1 && rest[0] == "pulls",
		method == http.MethodGet && len(rest) == 2 && rest[0] == "pulls",
		method == http.MethodGet && len(rest) == 3 && rest[0] == "pulls" && rest[2] == "files":
		return read("pull_requests")
	case method == http.MethodPost && len(rest) == 1 && rest[0] == "pulls":
		return map[string]string{"pull_requests": "write"}
	case method == http.MethodPut && len(rest) == 3 && rest[0] == "pulls" && rest[2] == "merge":
		return map[string]string{"pull_requests": "write", "contents": "write"}
	case method == http.MethodGet && len(rest) == 1 && rest[0] == "commits",
		method == http.MethodGet && len(rest) == 2 && rest[0] == "compare":
		return read("contents")
	case method == http.MethodGet && len(rest) == 3 && rest[0] == "commits" && rest[2] == "statuses":
		return read("statuses")
	case method == http.MethodGet && len(rest) == 3 && rest[0] == "commits" && (rest[2] == "check-suites" || rest[2] == "check-runs"):
		return read("checks")
	case method == http.MethodGet && len(rest) >= 2 && rest[0] == "commits":
		return read("contents")
	case method == http.MethodGet && len(rest) == 3 && rest[0] == "branches" && rest[2] == "protection":
		return read("administration")
	case method == http.MethodGet && len(rest) >= 2 && rest[0] == "branches":
		return read("contents")
	case method == http.MethodGet && len(rest) == 3 && rest[0] == "rules" && rest[1] == "branches":
		return read("metadata")
	case method == http.MethodGet && len(rest) == 2 && rest[0] == "issues":
		return read("issues")
	case method == http.MethodGet && len(rest) == 2 && rest[0] == "dependabot" && rest[1] == "alerts":
		return read("vulnerability_alerts")
	}
	return nil
}

// authorize checks the installation token the way GitHub does and returns the repository, or
// answers the request itself and returns nil.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, segs []string, needs map[string]string) *Repository {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	tok, known := s.tokens[raw]
	repo := s.repos[lower(segs[1], segs[2])]
	s.mu.Unlock()
	if !ok || !known || time.Now().After(tok.expires) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return nil
	}
	if tok.repo != lower(segs[1], segs[2]) || repo == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return nil
	}
	for name, want := range needs {
		have := tok.perms[name]
		if have != "write" && have != want {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
			return nil
		}
	}
	return repo
}

// tokenHas reports whether the request's installation token carries the permission at any level.
func (s *Server) tokenHas(r *http.Request, name string) bool {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[raw].perms[name] != ""
}

func (s *Server) repoCall(w http.ResponseWriter, r *http.Request, segs []string) {
	needs := need(r.Method, segs)
	if needs == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	repo := s.authorize(w, r, segs, needs)
	if repo == nil {
		return
	}
	rest := segs[3:]
	if r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "pulls" && !s.tokenHas(r, "contents") {
		// GitHub checks the head branch with the token and, without Contents access, cannot see
		// it: a private repository answers 422, not 403.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"resource": "PullRequest", "field": "head", "code": "invalid"}},
		})
		return
	}
	switch {
	case len(rest) == 0:
		s.mu.Lock()
		id := repo.ID
		if id == 0 {
			id = 1
		}
		out := map[string]any{
			"id": id, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"owner":       map[string]any{"login": repo.Owner, "avatar_url": repo.AvatarURL},
			"description": repo.Description, "default_branch": repo.DefaultBranch,
			"html_url":          "https://github.com/" + repo.Owner + "/" + repo.Name,
			"open_issues_count": repo.Issues + openCount(repo),
			"archived":          repo.Archived,
		}
		if !repo.OmitSquash && (s.tokenPerm(r, "contents") != "") {
			out["allow_squash_merge"] = !repo.NoSquash
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	case rest[0] == "pulls":
		s.pullCall(w, r, repo, rest)
	case rest[0] == "commits" && len(rest) == 1:
		s.commits(w, r, repo)
	case rest[0] == "commits" && len(rest) == 3 && (rest[2] == "statuses" || rest[2] == "check-suites" || rest[2] == "check-runs"):
		s.commitFacts(w, r, repo, rest[1], rest[2])
	case rest[0] == "commits":
		s.commitCall(w, repo, strings.Join(rest[1:], "/"))
	case rest[0] == "branches":
		s.branchCall(w, r, repo, rest[1:])
	case rest[0] == "rules":
		s.ruleCall(w, r, repo)
	case rest[0] == "compare" && len(rest) == 2:
		s.compareCall(w, r, repo, rest[1])
	case rest[0] == "issues" && len(rest) == 2:
		s.issueCall(w, repo, rest[1])
	case rest[0] == "dependabot" && len(rest) == 2 && rest[1] == "alerts":
		s.dependabotCall(w, r, repo)
	}
}

func page(r *http.Request, total int) (from, to, pageNo, last int) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per < 1 || per > 100 {
		per = 30
	}
	pageNo, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if pageNo < 1 {
		pageNo = 1
	}
	last = max((total+per-1)/per, 1)
	from = min((pageNo-1)*per, total)
	to = min(from+per, total)
	return from, to, pageNo, last
}

func (s *Server) link(w http.ResponseWriter, r *http.Request, pageNo, last int) {
	if last <= 1 {
		return
	}
	u := func(p int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p))
		return fmt.Sprintf("<%s%s?%s>", s.URL[:len(s.URL)-1], r.URL.Path, q.Encode())
	}
	var parts []string
	if pageNo < last {
		parts = append(parts, u(pageNo+1)+`; rel="next"`, u(last)+`; rel="last"`)
	}
	if pageNo > 1 {
		parts = append(parts, u(1)+`; rel="first"`, u(pageNo-1)+`; rel="prev"`)
	}
	w.Header().Set("Link", strings.Join(parts, ", "))
}

func (s *Server) commits(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	all := append([]Commit(nil), repo.Commits...)
	s.mu.Unlock()
	from, to, pageNo, last := page(r, len(all))
	out := []map[string]any{}
	for _, c := range all[from:to] {
		out = append(out, map[string]any{
			"sha":      c.SHA,
			"html_url": fmt.Sprintf("https://github.com/%s/%s/commit/%s", repo.Owner, repo.Name, c.SHA),
			"commit": map[string]any{
				"message": c.Message,
				"author":  map[string]any{"name": c.Author, "date": c.Date.UTC().Format(time.RFC3339)},
			},
		})
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

// tokenPerm is the level the request's token holds for a permission, or "".
func (s *Server) tokenPerm(r *http.Request, name string) string {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	return s.tokens[raw].perms[name]
}

// TokenAllows reports whether value is a live token the fake minted for owner/name that grants
// permission at read level or better. A test git server uses it to judge the Basic password a
// proxy sends, the way real GitHub judges an installation token on a git request.
func (s *Server) TokenAllows(value, owner, name, permission string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.tokens[value]
	if !ok || time.Now().After(tok.expires) || tok.repo != lower(owner, name) {
		return false
	}
	lvl := tok.perms[permission]
	return lvl == "read" || lvl == "write"
}
