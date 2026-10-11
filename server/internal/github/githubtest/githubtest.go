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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
}

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Query  string
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
}

// New starts a fake and closes it when t finishes.
func New(t testing.TB) *Server {
	t.Helper()
	loadKey()
	s := &Server{repos: map[string]*Repository{}, tokens: map[string]token{}, failures: map[string]int{}, redirects: map[string]string{}}
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
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
	status := s.failures[r.Method+" "+r.URL.Path]
	target := s.redirects[r.URL.Path]
	s.mu.Unlock()
	if target != "" {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if status != 0 {
		writeJSON(w, status, map[string]string{"message": SecretMarker})
		return
	}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(segs) == 4 && segs[0] == "repos" && segs[3] == "installation":
		s.installation(w, r, segs[1], segs[2])
	case r.Method == http.MethodPost && len(segs) == 4 && segs[0] == "app" && segs[1] == "installations" && segs[3] == "access_tokens":
		s.mint(w, r, segs[2])
	case r.Method == http.MethodGet && len(segs) >= 3 && segs[0] == "repos":
		s.repoRead(w, r, segs)
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

// need is the permission an endpoint requires.
func need(segs []string) string {
	switch {
	case len(segs) == 3:
		return "metadata"
	case len(segs) == 4 && segs[3] == "pulls":
		return "pull_requests"
	case len(segs) == 4 && segs[3] == "commits":
		return "contents"
	}
	return ""
}

func (s *Server) repoRead(w http.ResponseWriter, r *http.Request, segs []string) {
	perm := need(segs)
	if perm == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	tok, known := s.tokens[raw]
	repo := s.repos[lower(segs[1], segs[2])]
	s.mu.Unlock()
	if !ok || !known || time.Now().After(tok.expires) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	if tok.repo != lower(segs[1], segs[2]) || repo == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if lvl := tok.perms[perm]; lvl != "read" && lvl != "write" {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
		return
	}
	switch perm {
	case "metadata":
		s.mu.Lock()
		out := map[string]any{
			"id": 1, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"owner":       map[string]any{"login": repo.Owner, "avatar_url": repo.AvatarURL},
			"description": repo.Description, "default_branch": repo.DefaultBranch,
			"html_url":          "https://github.com/" + repo.Owner + "/" + repo.Name,
			"open_issues_count": repo.Issues + len(repo.PullRequests),
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	case "pull_requests":
		s.pulls(w, r, repo)
	case "contents":
		s.commits(w, r, repo)
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

func (s *Server) pulls(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	all := append([]PullRequest(nil), repo.PullRequests...)
	s.mu.Unlock()
	from, to, pageNo, last := page(r, len(all))
	out := []map[string]any{}
	for _, p := range all[from:to] {
		out = append(out, map[string]any{
			"number": p.Number, "title": p.Title, "state": "open",
			"user":       map[string]any{"login": p.Author},
			"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, p.Number),
			"head":     map[string]any{"ref": p.HeadRef},
		})
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
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
