package gittransport_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const (
	repoA = "oldsj/alpha"
	repoB = "oldsj/beta"
)

// gitEnv wires the real enroller, authenticator and proxy to a database, a fake cluster, a fake
// GitHub API and a bare repository served by git itself standing in for GitHub's git host.
type gitEnv struct {
	t        *testing.T
	auth     *authtest.Env
	enroller *gittransport.Enroller
	proxy    *httptest.Server
	srv      *gittransport.Server
	fake     *githubtest.Server
	gitBin   string
	root     string // upstream bare repositories: <root>/<owner>/<name>.git

	mu       sync.Mutex
	upstream []http.Header // headers of the requests the upstream saw
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	d := testdb.New(t)
	e := &gitEnv{t: t, gitBin: gitBin, auth: authtest.NewEnv(d.Pool), fake: githubtest.New(t), root: t.TempDir()}
	for _, r := range []string{repoA, repoB} {
		owner, name, _ := strings.Cut(r, "/")
		e.fake.AddRepo(githubtest.Repository{Owner: owner, Name: name})
		e.initUpstream(owner, name)
	}
	// The upstream: git http-backend behind the same check GitHub makes of an installation token.
	backend := &cgi.Handler{
		Path: gitBin, Args: []string{"http-backend"}, Root: "/",
		Env: []string{"GIT_PROJECT_ROOT=" + e.root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.upstream = append(e.upstream, r.Header.Clone())
		e.mu.Unlock()
		user, pass, ok := r.BasicAuth()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if !ok || user != "x-access-token" || len(parts) < 2 ||
			!e.fake.TokenAllows(pass, parts[0], strings.TrimSuffix(parts[1], ".git"), "contents") {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		// The proxy sends the body chunked, which GitHub accepts; Go's CGI bridge would hand
		// http-backend no length, so the stand-in buffers it.
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(buf)), int64(len(buf)), nil
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	app, err := github.NewApp(github.Options{
		AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: e.fake.URL, GitBaseURL: up.URL + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.enroller = &gittransport.Enroller{Pool: d.Pool, Creds: e.auth.Creds}
	e.srv = &gittransport.Server{
		Auth:     &gittransport.Authenticator{Q: dbq.New(d.Pool)},
		Upstream: app,
		Log:      slog.New(slog.NewTextHandler(testLog{t}, nil)),
		Ready:    d.Pool,
		Dev:      true,
		AllowedHosts: map[string]bool{
			"mainloop-git-read.mainloop.svc.cluster.local": true,
		},
	}
	e.proxy = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.proxy.Close)
	return e
}

func (e *gitEnv) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.CommandContext(context.Background(), e.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(e.gitEnvVars(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *gitEnv) initUpstream(owner, name string) {
	e.t.Helper()
	bare := filepath.Join(e.root, owner, name+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.git(bare, "init", "--bare", "-b", "main")
	e.commitUpstream(owner, name, "first.txt", "one")
}

// commitUpstream adds a commit to the upstream's main branch, the way a push to GitHub would.
func (e *gitEnv) commitUpstream(owner, name, file, content string) {
	e.t.Helper()
	bare := filepath.Join(e.root, owner, name+".git")
	work := e.t.TempDir()
	e.git(work, "init", "-b", "main")
	if _, err := os.Stat(filepath.Join(bare, "refs", "heads", "main")); err == nil {
		e.git(work, "pull", bare, "main")
	}
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.git(work, "add", file)
	e.git(work, "commit", "-m", "add "+file)
	e.git(work, "push", bare, "main")
}

// workspace seeds a live workspace on repo and returns its id.
func (e *gitEnv) workspace(repo string) string {
	e.t.Helper()
	s := e.auth.Seed(e.t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p-` + s.SessionID + `', 'owner', 'o', 'n', 'o/` + s.SessionID + `', 'https://x')`,
		`INSERT INTO workspaces (id, user_id, project_id, repo_url, branch, agent_kind, agent_namespace, agent_name, request_id)
		 VALUES ('` + s.SessionID + `', 'owner', 'p-` + s.SessionID + `', 'https://github.com/` + repo + `', 'b', 'claude', 'kagent', 'claude', 'req-` + s.SessionID + `')`,
	} {
		if _, err := e.auth.Pool.Exec(ctx, stmt); err != nil {
			e.t.Fatalf("seed workspace: %v", err)
		}
	}
	return s.SessionID
}

// revoke does what ending a workspace does to its read capability: the credential service revokes
// the credential, and the enrollment row follows (here at once rather than through the sweep).
func (e *gitEnv) revoke(workspaceID string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.auth.Creds.Revoke(ctx, workspaceID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := dbq.New(e.auth.Pool).RevokeGitEnrollment(ctx, workspaceID); err != nil {
		e.t.Fatal(err)
	}
}

// capability enrolls a workspace and returns its bearer, the way the agent's egress would send it.
func (e *gitEnv) capability(workspaceID string) string {
	e.t.Helper()
	en, err := e.enroller.Enroll(context.Background(), workspaceID)
	if err != nil {
		e.t.Fatalf("enroll: %v", err)
	}
	return agentauth.Token(authtest.Key, en.CredentialID)
}

func (e *gitEnv) clone(bearer, repo, dir string) (string, error) {
	cmd := exec.CommandContext(context.Background(), e.gitBin, "-c", "http.extraHeader=Authorization: Bearer "+bearer, "clone", e.proxy.URL+"/"+repo+".git", dir)
	cmd.Env = e.gitEnvVars()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// do sends a raw request to the proxy.
func (e *gitEnv) do(method, path, bearer string, hdr map[string]string, body io.Reader) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.proxy.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *gitEnv) upstreamSaw() []http.Header {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]http.Header(nil), e.upstream...)
}

func TestCloneAndFetchThroughTheProxy(t *testing.T) {
	e := newGitEnv(t)
	bearer := e.capability(e.workspace(repoA))
	dir := filepath.Join(t.TempDir(), "clone")
	if out, err := e.clone(bearer, repoA, dir); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "first.txt")); err != nil || string(got) != "one" {
		t.Fatalf("cloned file = %q, %v", got, err)
	}

	e.commitUpstream("oldsj", "alpha", "second.txt", "two")
	e.git(dir, "-c", "http.extraHeader=Authorization: Bearer "+bearer, "fetch", "origin")
	if got := e.git(dir, "show", "origin/main:second.txt"); got != "two" {
		t.Fatalf("fetched file = %q", got)
	}

	// GitHub only ever saw the installation token, never the agent's capability, and the caller's
	// header did not travel.
	saw := e.upstreamSaw()
	if len(saw) == 0 {
		t.Fatal("the upstream saw nothing")
	}
	for _, h := range saw {
		auth := h.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") || strings.Contains(auth, bearer) {
			t.Fatalf("upstream Authorization = %q", auth)
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-") {
				t.Fatalf("upstream saw header %q", k)
			}
		}
	}
	// The token was scoped to the one repository and to read.
	for _, m := range e.fake.Mints() {
		if len(m.Repositories) != 1 || m.Repositories[0] != "alpha" || m.Permissions["contents"] != "read" || len(m.Permissions) != 1 {
			t.Fatalf("mint = %+v", m)
		}
	}
}

func TestProxyRefusals(t *testing.T) {
	e := newGitEnv(t)
	bearerA := e.capability(e.workspace(repoA))
	bearerB := e.capability(e.workspace(repoB))
	disc := "/oldsj/alpha.git/info/refs?service=git-upload-pack"
	post := "/oldsj/alpha.git/git-upload-pack"
	reqType := map[string]string{"Content-Type": "application/x-git-upload-pack-request"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	cases := []struct {
		name   string
		method string
		path   string
		bearer string
		hdr    map[string]string
		status int
		code   string
	}{
		{"no capability", "GET", disc, "", nil, 401, "authentication_required"},
		{"unknown capability", "GET", disc, "ml_" + strings.Repeat("0", 64), nil, 401, "authentication_required"},
		{"foreign bearer", "GET", disc, "ghs_something", nil, 401, "authentication_required"},
		{"another workspace's capability", "GET", disc, bearerB, nil, 403, "repository_denied"},
		{"another repository", "GET", "/oldsj/beta.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "repository_denied"},
		{"repository case is not a way round", "GET", "/OLDSJ/Beta.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "repository_denied"},
		{"receive-pack advertisement", "GET", "/oldsj/alpha.git/info/refs?service=git-receive-pack", bearerA, nil, 403, "push_not_served"},
		{"receive-pack post", "POST", "/oldsj/alpha.git/git-receive-pack", bearerA, with(map[string]string{"Content-Type": "application/x-git-receive-pack-request"}), 403, "push_not_served"},
		{"unknown service", "GET", "/oldsj/alpha.git/info/refs?service=git-upload-archive", bearerA, nil, 403, "route_denied"},
		{"no service", "GET", "/oldsj/alpha.git/info/refs", bearerA, nil, 403, "route_denied"},
		{"extra query", "GET", disc + "&x=1", bearerA, nil, 403, "route_denied"},
		{"wrong method on discovery", "POST", disc, bearerA, reqType, 403, "route_denied"},
		{"wrong method on upload-pack", "GET", post, bearerA, nil, 403, "route_denied"},
		{"unknown path", "GET", "/oldsj/alpha.git/HEAD", bearerA, nil, 403, "path_invalid"},
		{"encoded path", "GET", "/oldsj/alpha%2e.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "path_invalid"},
		{"traversal", "GET", "/oldsj/../alpha.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "path_invalid"},
		{"doubled suffix", "GET", "/oldsj/alpha.git.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "path_invalid"},
		{"upper-case suffix", "GET", "/oldsj/alpha.GIT.git/info/refs?service=git-upload-pack", bearerA, nil, 403, "path_invalid"},
		{"root", "GET", "/", bearerA, nil, 403, "path_invalid"},
		{"bad Host", "GET", disc, bearerA, map[string]string{"Host": "evil.example"}, 404, ""},
		{"forwarded header", "GET", disc, bearerA, map[string]string{"X-Forwarded-For": "10.0.0.1"}, 403, "routing_header"},
		{"underscore forwarded header", "GET", disc, bearerA, map[string]string{"x_forwarded_for": "10.0.0.1"}, 403, "routing_header"},
		{"real IP header", "GET", disc, bearerA, map[string]string{"X-Real-Ip": "10.0.0.1"}, 403, "routing_header"},
		{"Via header", "GET", disc, bearerA, map[string]string{"Via": "proxy"}, 403, "routing_header"},
		{"Envoy header", "GET", disc, bearerA, map[string]string{"X-Envoy-External-Address": "10.0.0.1"}, 403, "routing_header"},
		{"actor header", "GET", disc, bearerA, map[string]string{"X-Actor-Id": "someone"}, 403, "routing_header"},
		{"session header", "GET", disc, bearerA, map[string]string{"X-Session-Id": "s"}, 403, "routing_header"},
		{"Forwarded header", "GET", disc, bearerA, map[string]string{"Forwarded": "for=1.2.3.4"}, 403, "routing_header"},
		{"method override", "GET", disc, bearerA, map[string]string{"X-HTTP-Method-Override": "POST"}, 403, "routing_header"},
		{"proxy authorization", "GET", disc, bearerA, map[string]string{"Proxy-Authorization": "Basic eDp5"}, 403, "routing_header"},
		{"upgrade", "GET", disc, bearerA, map[string]string{"Upgrade": "websocket"}, 403, "routing_header"},
		{"unsupported content encoding", "POST", post, bearerA, with(map[string]string{"Content-Type": "application/x-git-upload-pack-request", "Content-Encoding": "br"}), 403, "http_compression"},
		{"wrong content type", "POST", post, bearerA, map[string]string{"Content-Type": "text/plain"}, 403, "content_type"},
		{"bad git protocol", "GET", disc, bearerA, map[string]string{"Git-Protocol": "version=9"}, 403, "git_protocol"},
		{"git protocol injection", "GET", disc, bearerA, map[string]string{"Git-Protocol": "version=2\r\nX-Evil: 1"}, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(e.upstreamSaw())
			if tc.status == 0 {
				// A CRLF in a header value never reaches the wire: the client refuses to send it.
				req, _ := http.NewRequestWithContext(context.Background(), tc.method, e.proxy.URL+tc.path, http.NoBody)
				req.Header.Set("Git-Protocol", "version=2\r\nX-Evil: 1")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("a header with a line break was sent")
				}
				return
			}
			status, body := e.do(tc.method, tc.path, tc.bearer, tc.hdr, http.NoBody)
			if status != tc.status || (tc.code != "" && !strings.HasPrefix(body, tc.code+" ")) {
				t.Fatalf("got %d %q, want %d %q", status, body, tc.status, tc.code)
			}
			if got := len(e.upstreamSaw()); got != before {
				t.Fatalf("a refused request reached the upstream (%d -> %d requests)", before, got)
			}
		})
	}

	t.Run("duplicate Authorization", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", e.proxy.URL+disc, http.NoBody)
		req.Header.Add("Authorization", "Bearer "+bearerA)
		req.Header.Add("Authorization", "Bearer "+bearerB)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 403 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("valid", func(t *testing.T) {
		status, body := e.do("GET", disc, bearerA, nil, http.NoBody)
		if status != 200 || !strings.Contains(body, "refs/heads/main") {
			t.Fatalf("got %d %q", status, body)
		}
	})
}

func TestRevokedCapabilityStopsWorking(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	bearer := e.capability(ws)
	disc := "/oldsj/alpha.git/info/refs?service=git-upload-pack"
	if status, _ := e.do("GET", disc, bearer, nil, http.NoBody); status != 200 {
		t.Fatalf("status %d", status)
	}
	e.revoke(ws)
	if status, body := e.do("GET", disc, bearer, nil, http.NoBody); status != 401 {
		t.Fatalf("after revoke: %d %q", status, body)
	}
	en, err := dbq.New(e.auth.Pool).GetGitEnrollment(context.Background(), ws)
	if err != nil || en.State != dbstate.GitEnrollmentRevoked || en.RevokedAt == nil {
		t.Fatalf("enrollment = %+v, %v", en, err)
	}
	// The Secret goes with the credential, through the same cleanup as any agent credential.
	name := e.auth.Creds.SecretName(dbstate.CredentialGitRead, ws)
	if e.auth.Cluster.UID(authtest.Namespace, name) == "" {
		t.Fatal("the capability's Secret was never created")
	}
	runCleanup(t, e, func() bool { return e.auth.Cluster.UID(authtest.Namespace, name) == "" })
}

func TestEndedWorkspaceLosesItsCapability(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	bearer := e.capability(ws)
	disc := "/oldsj/alpha.git/info/refs?service=git-upload-pack"
	// The session ends and nothing has revoked the credential yet: the capability is still dead.
	if _, err := e.auth.Pool.Exec(context.Background(), `UPDATE sessions SET status = 'cancelled' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do("GET", disc, bearer, nil, http.NoBody); status != 401 {
		t.Fatalf("status %d", status)
	}
	if _, err := e.enroller.Enroll(context.Background(), ws); err == nil {
		t.Fatal("an ended workspace was enrolled")
	}
}

func runCleanup(t *testing.T, e *gitEnv, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := reconcile.NewRunner(e.auth.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	e.auth.Creds.Register(r)
	e.enroller.Register(r)
	finished := make(chan error, 1)
	go func() { finished <- r.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-finished
}

// testLog sends the server's log to the test's output.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// gitEnvVars is a minimal environment for the git binary: a throwaway HOME so no user
// configuration applies, and no terminal prompts.
func (e *gitEnv) gitEnvVars() []string {
	return []string{
		"PATH=" + filepath.Dir(e.gitBin) + ":/usr/bin:/bin", "HOME=" + e.t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	}
}

// The publication ledger outlives the workspace: deleting a session whose workspace has a push
// grant and a publication must succeed and leave the publication behind.
func TestDeletingASessionKeepsItsPublications(t *testing.T) {
	e := newGitEnv(t)
	ws := e.workspace(repoA)
	ctx := context.Background()
	if _, err := e.enroller.Enroll(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO git_push_grants (workspace_id, branch, policy_version, credential_id, state)
		 VALUES ('` + ws + `', 'b', 1, 'c1', 'published')`,
		`INSERT INTO git_publications (id, workspace_id, request_id, repository, branch, attempt)
		 VALUES ('pub1', '` + ws + `', 'r1', 'oldsj/alpha', 'b', '{}')`,
	} {
		if _, err := e.auth.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.auth.Pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, ws); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var n, grants int
	if err := e.auth.Pool.QueryRow(ctx, `SELECT count(*) FROM git_publications WHERE id = 'pub1' AND workspace_id = $1`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("publication rows = %d, %v", n, err)
	}
	if err := e.auth.Pool.QueryRow(ctx, `SELECT count(*) FROM git_push_grants WHERE workspace_id = $1`, ws).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grant rows = %d, %v", grants, err)
	}
}
