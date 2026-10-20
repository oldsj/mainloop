package pushgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets/secretstest"
	"github.com/oldsj/mainloop/server/internal/testdb"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

type nativeFake struct {
	mu      sync.Mutex
	session *ka.Session
	before  func()
}

func (n *nativeFake) GetSession(context.Context, string) (*ka.Session, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.before != nil {
		n.before()
	}
	return proto.Clone(n.session).(*ka.Session), nil
}

type upstreamSpy struct {
	Upstream
	mu       sync.Mutex
	calls    int
	pushes   int
	lost     bool
	failSeed bool
	reply    []byte
	before   func(string)
}

func (u *upstreamSpy) mark(stage string) {
	u.mu.Lock()
	u.calls++
	f := u.before
	u.mu.Unlock()
	if f != nil {
		f(stage)
	}
}
func (u *upstreamSpy) Refs(ctx context.Context, r github.Repo) (map[string]string, error) {
	u.mark("refs")
	return u.Upstream.Refs(ctx, r)
}
func (u *upstreamSpy) Seed(ctx context.Context, r github.Repo, w []string, out io.Writer) error {
	u.mark("seed")
	u.mu.Lock()
	fail := u.failSeed
	u.mu.Unlock()
	if fail {
		return denial("upstream_unavailable")
	}
	return u.Upstream.Seed(ctx, r, w, out)
}
func (u *upstreamSpy) DefaultBranch(ctx context.Context, r github.Repo) (string, error) {
	u.mark("metadata")
	return u.Upstream.DefaultBranch(ctx, r)
}
func (u *upstreamSpy) Push(ctx context.Context, r github.Repo, b io.Reader, workflows bool) ([]byte, error) {
	u.mark("push")
	u.mu.Lock()
	u.pushes++
	lost := u.lost
	reply := u.reply
	u.mu.Unlock()
	if reply != nil {
		return reply, nil
	}
	out, err := u.Upstream.Push(ctx, r, b, workflows)
	if err == nil && lost {
		return nil, errors.New("lost upstream-secret-marker")
	}
	return out, err
}
func (u *upstreamSpy) counts() (int, int) { u.mu.Lock(); defer u.mu.Unlock(); return u.calls, u.pushes }

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	root     string
	bare     string
	work     string
	base     string
	head     string
	token    string
	creds    *agentauth.Credentials
	native   *nativeFake
	upstream *upstreamSpy
	svc      *Service
	server   *Server
	proxy    *httptest.Server
	fake     *githubtest.Server
	logs     bytes.Buffer
}

func newEnv(t *testing.T) *env {
	return newEnvForRole(t, dbstate.RoleAgent)
}
func newEnvForRole(t *testing.T, role dbstate.BindingRole) *env {
	t.Helper()
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(cache, "mainloop-push-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	d := testdb.New(t)
	e := &env{t: t, pool: d.Pool, root: root, fake: githubtest.New(t)}
	e.work = filepath.Join(root, "work")
	e.bare = filepath.Join(root, "upstream", "oldsj", "alpha.git")
	for _, p := range []string{e.work, e.bare} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	e.git(e.bare, "init", "--bare", "-b", "main")
	e.git(e.bare, "config", "http.receivepack", "true")
	e.git(e.work, "init", "-b", "main")
	e.commit("first")
	e.base = e.git(e.work, "rev-parse", "HEAD")
	e.git(e.work, "push", e.bare, "main")
	e.git(e.work, "checkout", "-b", "feature/test")
	e.commit("second")
	e.head = e.git(e.work, "rev-parse", "HEAD")
	e.fake.AddRepo(githubtest.Repository{Owner: "oldsj", Name: "alpha", DefaultBranch: "main"})
	backend := &cgi.Handler{Path: "/usr/bin/git", Args: []string{"http-backend"}, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "upstream"), "GIT_HTTP_EXPORT_ALL=1"}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || !e.fake.TokenAllows(pass, "oldsj", "alpha", "contents") {
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "git-receive-pack") {
			allowed := false
			for _, mint := range e.fake.Mints() {
				if mint.Permissions["contents"] == "write" {
					allowed = true
				}
			}
			if !allowed {
				w.WriteHeader(403)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	remote, err := NewGitHubUpstream(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: e.fake.URL, GitBaseURL: up.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	e.upstream = &upstreamSpy{Upstream: remote}
	e.exec(`INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ('p','owner','oldsj','alpha','oldsj/alpha','https://github.com/oldsj/alpha')`)
	e.exec(`INSERT INTO sessions (id,user_id,project_id,title,description,prompt,status,repo_url,branch_name) VALUES ('ws','owner','p','t','d','p','active','https://github.com/oldsj/alpha','feature/test')`)
	e.exec(`INSERT INTO agent_bindings (session_id,kind,role,mcp_grant_kind,kagent_session_id) VALUES ('ws','claude','agent','workspace','native')`)
	e.exec(`INSERT INTO workspaces (id,user_id,project_id,repo_url,ref,branch,agent_kind,agent_namespace,agent_name,request_id,environment) VALUES ('ws','owner','p','https://github.com/oldsj/alpha',$1,'feature/test','claude','kagent','claude','create-ws','{"environment_id":"env","version_id":"v","image":"example.test/dev@sha256:abc","platform":"linux/amd64","policy_identity":"policy"}')`, e.base)
	e.exec(`INSERT INTO workspace_writer_claims (owner_id,repository,branch,generation,binding_id) VALUES ('owner','oldsj/alpha','feature/test',1,'ws')`)
	if role != dbstate.RoleAgent {
		e.exec(`UPDATE agent_bindings SET role=$1 WHERE session_id='ws'`, role)
		root, parent, depth := "task", (*string)(nil), 1
		if role == dbstate.RoleChild {
			root = "parent-task"
			parent = ptr(root)
			depth = 2
			e.exec(`INSERT INTO sessions (id,user_id,project_id,title,description,prompt,status) VALUES ('parent-ws','owner','p','t','d','p','active')`)
			e.exec(`INSERT INTO agent_bindings (session_id,kind,role,mcp_grant_kind,kagent_session_id) VALUES ('parent-ws','claude','supervisor','workspace','parent-native')`)
			e.exec(`INSERT INTO tasks (id,owner_id,project_id,root_task_id,title,brief,mode,assigned_profile_id,selection_source,status,checkout_branch,checkout_ref,checkout_depth)
				VALUES ('parent-task','owner','p','parent-task','t','b','code','claude','explicit','running','feature/parent',$1,0)`, e.base)
			e.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,state,binding_id,session_id,workspace_id,writer_generation)
				VALUES ('parent-attempt','parent-task',1,'claude','claude','rev','kagent','claude','supervisor',1,'active','parent-ws','parent-ws','parent-ws',1)`)
			e.exec(`UPDATE tasks SET current_attempt_id='parent-attempt' WHERE id='parent-task'`)
		}
		e.exec(`INSERT INTO tasks (id,owner_id,project_id,root_task_id,parent_task_id,title,brief,mode,assigned_profile_id,selection_source,status,checkout_branch,checkout_ref,checkout_depth)
			VALUES ('task','owner','p',$2,$3,'t','b','code','claude','explicit','running','feature/test',$1,0)`, e.base, root, parent)
		e.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,state,binding_id,session_id,workspace_id,writer_generation)
			VALUES ('attempt','task',1,'claude','claude','rev','kagent','claude',$1,$2,'active','ws','ws','ws',2)`, role, depth)
		e.exec(`UPDATE tasks SET current_attempt_id='attempt' WHERE id='task'`)
		e.exec(`UPDATE workspace_writer_claims SET held=false`)
		e.exec(`UPDATE workspace_writer_claims SET held=true,generation=2,attempt_id='attempt',binding_id=NULL`)
		if role == dbstate.RoleChild {
			e.exec(`INSERT INTO workspace_writer_claims (owner_id,repository,branch,generation,attempt_id) VALUES ('owner','oldsj/alpha','feature/parent',1,'parent-attempt')`)
		}
	}
	cluster := secretstest.New()
	e.creds = agentauth.NewCredentials(e.pool, k8ssecrets.New(cluster, "kagent"), []byte("fixture-hmac-key-only"), "mainloop", nil)
	if _, err = (&gittransport.Enroller{Pool: e.pool, Creds: e.creds}).Enroll(context.Background(), "ws"); err != nil {
		t.Fatal(err)
	}
	e.native = &nativeFake{session: &ka.Session{Id: "native", ContextId: "native", State: ka.RuntimeState_RUNTIME_STATE_READY, Operation: ka.RuntimeOperation_RUNTIME_OPERATION_NONE, PreparedRevision: "revision",
		Agent: &ka.ResourceReference{Namespace: "kagent", Name: "claude"}, Workspace: &ka.Workspace{Repo: "https://github.com/oldsj/alpha", Ref: e.base, Branch: "feature/test"}, DevelopmentEnvironment: &ka.DevelopmentEnvironment{Image: "example.test/dev@sha256:abc", Platform: "linux/amd64", PolicyIdentity: "policy"},
		RuntimeComposition: &ka.RuntimeComposition{PayloadImage: "example.test/harness@sha256:def", Provider: "claude", Schema: 1}, RuntimeAssociation: &ka.RuntimeAssociation{GenerationId: "runtime-g1", ActorUid: "actor-uid", ActorName: "actor", Atespace: "space", Phase: "active", CurrentActive: true}}}
	e.svc = &Service{Pool: e.pool, Native: e.native, Upstream: e.upstream, Credentials: e.creds}
	ref := Reference(e.creds, "http://mainloop-git-push.mainloop.svc.cluster.local", "ws")
	issued, err := e.svc.Enroll(context.Background(), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if ref.GetSecretRef().GetName() != issued.SecretName {
		t.Fatal("reference changed")
	}
	again, err := e.svc.Enroll(context.Background(), "ws")
	if err != nil || again != issued {
		t.Fatalf("issuance replay: %v", err)
	}
	e.token = agentauth.Token(e.creds.Key, issued.CredentialID)
	e.server = &Server{Service: e.svc, Enabled: true, Dev: true, Log: slog.New(slog.NewJSONHandler(&e.logs, nil)), SpoolRoot: filepath.Join(root, "spool"), Limits: Limits{DiskBytes: 32 << 20, BodyBytes: 16 << 20, Timeout: 10 * time.Second}}
	e.proxy = httptest.NewServer(e.server.Handler())
	t.Cleanup(e.proxy.Close)
	return e
}
func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}
func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	return strings.TrimSpace(string(e.gitBytes(dir, nil, args...)))
}
func (e *env) gitBytes(dir string, in io.Reader, args ...string) []byte {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/git", args...)
	cmd.Dir = dir
	cmd.Stdin = in
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + e.root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.test"}
	var errout bytes.Buffer
	cmd.Stderr = &errout
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("git fixture failed: %v: %s", err, errout.String())
	}
	return out
}
func (e *env) commit(text string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.work, "file"), []byte(text), 0600); err != nil {
		e.t.Fatal(err)
	}
	e.git(e.work, "add", "file")
	e.git(e.work, "commit", "-m", text)
}
func (e *env) commitPath(path, text string) {
	e.t.Helper()
	full := filepath.Join(e.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0600); err != nil {
		e.t.Fatal(err)
	}
	e.git(e.work, "add", path)
	e.git(e.work, "commit", "-m", "commit "+path)
}
func mintedWorkflowsWrite(mints []githubtest.Mint) bool {
	for _, m := range mints {
		if m.Permissions["workflows"] == "write" {
			return true
		}
	}
	return false
}
func (e *env) body(old, new, ref string) []byte {
	e.t.Helper()
	revisions := new + "\n"
	if old != zeroOID {
		revisions += "^" + old + "\n"
	}
	pack := e.gitBytes(e.work, strings.NewReader(revisions), "pack-objects", "--stdout", "--revs", "--thin", "--delta-base-offset")
	b := pkt([]byte(old + " " + new + " " + ref + "\x00report-status ofs-delta\n"))
	b = append(b, []byte("0000")...)
	return append(b, pack...)
}
func (e *env) request(body []byte, mutate func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	r.Header.Set("Authorization", "Bearer "+e.token)
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(w, r)
	return w
}
func (e *env) state() dbstate.GitPublicationState {
	e.t.Helper()
	var state string
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM git_publications ORDER BY created_at DESC LIMIT 1`).Scan(&state); err != nil {
		e.t.Fatal(err)
	}
	return dbstate.GitPublicationState(state)
}
func (e *env) publicationCount() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM git_publications`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestPushCreationFastForwardAndReplay(t *testing.T) {
	e := newEnv(t)
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	w := e.request(body, nil)
	if w.Code != 200 || e.state() != dbstate.PublicationConfirmed {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	if e.git(e.bare, "rev-parse", "refs/heads/feature/test") != e.head {
		t.Fatal("remote did not move")
	}
	_, before := e.upstream.counts()
	w = e.request(body, nil)
	_, after := e.upstream.counts()
	if w.Code != 200 || before != after || e.publicationCount() != 1 {
		t.Fatal("replay sent upstream bytes")
	}
	old := e.head
	e.commit("third")
	e.head = e.git(e.work, "rev-parse", "HEAD")
	w = e.request(e.body(old, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 || e.git(e.bare, "rev-parse", "refs/heads/feature/test") != e.head {
		t.Fatalf("fast-forward: %d %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(e.server.SpoolRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("spool not cleaned")
	}
	var validated bool
	if err = e.pool.QueryRow(context.Background(), `SELECT (transport_evidence->>'validated')::boolean FROM git_publications ORDER BY created_at DESC LIMIT 1`).Scan(&validated); err != nil || !validated {
		t.Fatal("validation evidence absent")
	}
}

func TestExplicitRetryAfterPreDispatchFailure(t *testing.T) {
	e := newEnv(t)
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	e.upstream.failSeed = true
	w := e.request(body, nil)
	if w.Code == 200 || e.state() != dbstate.PublicationRejected {
		t.Fatalf("seed failure: %d %s", w.Code, w.Body.String())
	}
	e.upstream.failSeed = false
	w = e.request(body, nil)
	if w.Code != 200 || e.state() != dbstate.PublicationConfirmed || e.publicationCount() != 2 {
		t.Fatalf("explicit retry: %d %s", w.Code, w.Body.String())
	}
	_, pushes := e.upstream.counts()
	if pushes != 1 {
		t.Fatal("failed validation published")
	}
	var requestID string
	if err := e.pool.QueryRow(context.Background(), `SELECT request_id FROM git_publications WHERE state='confirmed'`).Scan(&requestID); err != nil || requestID != w.Header().Get("x-mainloop-request-id") {
		t.Fatal("correlation id not retained in ledger")
	}
}

func TestRealGitPushThroughGate(t *testing.T) {
	e := newEnv(t)
	e.git(e.work, "-c", "http.extraHeader=Authorization: Bearer "+e.token, "push", e.proxy.URL+"/oldsj/alpha.git", "HEAD:refs/heads/feature/test")
	e.commit("real fast-forward")
	e.git(e.work, "-c", "http.extraHeader=Authorization: Bearer "+e.token, "push", e.proxy.URL+"/oldsj/alpha.git", "HEAD:refs/heads/feature/test")
	if e.git(e.bare, "rev-parse", "refs/heads/feature/test") != e.git(e.work, "rev-parse", "HEAD") || e.publicationCount() != 2 {
		t.Fatal("git push failed")
	}
	for _, m := range e.fake.Mints() {
		if len(m.Repositories) != 1 || m.Repositories[0] != "alpha" {
			t.Fatal("token scope")
		}
	}
	if strings.Contains(e.logs.String(), e.token) || strings.Contains(e.logs.String(), "ghs_fake_") {
		t.Fatal("secret in logs")
	}
}

func TestPushWithoutWorkflowFilesKeepsContentsOnlyScope(t *testing.T) {
	e := newEnv(t)
	w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	if mintedWorkflowsWrite(e.fake.Mints()) {
		t.Fatal("push with no .github/workflows/ change requested the workflows permission")
	}
}

func TestBranchCreationTouchingWorkflowsRequestsWorkflowsPermission(t *testing.T) {
	e := newEnv(t)
	e.commitPath(".github/workflows/ci.yml", "jobs: {}")
	head := e.git(e.work, "rev-parse", "HEAD")
	w := e.request(e.body(zeroOID, head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	if !mintedWorkflowsWrite(e.fake.Mints()) {
		t.Fatal("new branch introducing a .github/workflows/ file did not request the workflows permission")
	}
}

func TestFastForwardTouchingWorkflowsRequestsWorkflowsPermission(t *testing.T) {
	e := newEnv(t)
	w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatalf("initial push: %d %s", w.Code, w.Body.String())
	}
	if mintedWorkflowsWrite(e.fake.Mints()) {
		t.Fatal("initial push with no .github/workflows/ change requested the workflows permission")
	}
	old := e.head
	e.commitPath(".github/workflows/ci.yml", "jobs: {}")
	e.head = e.git(e.work, "rev-parse", "HEAD")
	w = e.request(e.body(old, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatalf("fast-forward push: %d %s", w.Code, w.Body.String())
	}
	if !mintedWorkflowsWrite(e.fake.Mints()) {
		t.Fatal("fast-forward introducing a .github/workflows/ file did not request the workflows permission")
	}
}

// A push creating a branch that merely points at a commit the repository already has — even one
// whose tree contains a workflow file committed long before this push — introduces no new
// .github/workflows/ change and must not ask for the broader permission.
func TestBranchCreationAtKnownDefaultCommitSkipsWorkflowsPermission(t *testing.T) {
	e := newEnv(t)
	e.git(e.work, "checkout", "main")
	e.commitPath(".github/workflows/existing.yml", "jobs: {}")
	newMain := e.git(e.work, "rev-parse", "HEAD")
	e.git(e.work, "push", e.bare, "main")
	e.git(e.work, "checkout", "feature/test")
	second, token2 := additionalBranch(t, e, "ws2", "feature/other")
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack", bytes.NewReader(e.body(zeroOID, newMain, "refs/heads/feature/other")))
	r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	r.Header.Set("Authorization", "Bearer "+token2)
	w := httptest.NewRecorder()
	second.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	if mintedWorkflowsWrite(e.fake.Mints()) {
		t.Fatal("push at an already-known commit requested the workflows permission")
	}
}

func TestAdversarialRequests(t *testing.T) {
	tests := []struct {
		name   string
		change func(*env, []byte) ([]byte, func(*http.Request))
		code   string
	}{
		{"malformed pack", func(e *env, b []byte) ([]byte, func(*http.Request)) { b[len(b)-1] ^= 1; return b, nil }, "pack_checksum"},
		{"ref deletion", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			return append(pkt([]byte(e.head+" "+zeroOID+" refs/heads/feature/test\x00report-status")), []byte("0000")...), nil
		}, "deletion"},
		{"non fast forward", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			e.git(e.work, "push", e.bare, "HEAD:feature/test")
			return e.body(e.head, e.base, "refs/heads/feature/test"), nil
		}, "non_fast_forward"},
		{"protected branch", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			e.exec(`UPDATE git_branch_policies SET policy = policy || '{"protected_patterns":["feature/*"]}'`)
			return b, nil
		}, "protected_branch"},
		{"two refs", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			a := pkt([]byte(zeroOID + " " + e.head + " refs/heads/feature/test\x00report-status"))
			a = append(a, pkt([]byte(zeroOID+" "+e.head+" refs/heads/other"))...)
			return append(a, []byte("0000")...), nil
		}, "single_ref_required"},
		{"gzip body", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			return b, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }
		}, "http_compression"},
		{"header injection", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			return b, func(r *http.Request) { r.Header.Set("X-Actor-Identity", "untrusted") }
		}, "routing_header"},
		{"stale writer generation", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			e.exec(`UPDATE workspace_writer_claims SET held=false`)
			e.exec(`UPDATE workspace_writer_claims SET held=true,generation=2`)
			return b, nil
		}, "stale_writer_generation"},
		{"changed runtime", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			e.native.session.RuntimeAssociation.ActorUid = "replacement"
			return b, nil
		}, "runtime_changed"},
		{"another repository", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			return b, func(r *http.Request) { r.URL.Path = "/oldsj/beta.git/git-receive-pack" }
		}, "repository_mismatch"},
		{"bad host", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			return b, func(r *http.Request) { r.Host = "evil.example.test" }
		}, "host_denied"},
		{"default branch", func(e *env, b []byte) ([]byte, func(*http.Request)) {
			e.exec(`UPDATE projects SET default_branch='feature/test'`)
			e.exec(`UPDATE git_branch_policies SET policy='{"default_branch":"feature/test"}'`)
			return b, nil
		}, "default_branch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			b, mutate := test.change(e, e.body(zeroOID, e.head, "refs/heads/feature/test"))
			_, before := e.upstream.counts()
			w := e.request(b, mutate)
			_, after := e.upstream.counts()
			if w.Code == 200 || !strings.HasPrefix(w.Body.String(), test.code+" ") {
				t.Fatalf("denial: %d %s", w.Code, w.Body.String())
			}
			if after != before {
				t.Fatal("refused push dispatched")
			}
			if e.publicationCount() > 0 && e.state() != dbstate.PublicationRejected {
				t.Fatal("pre-dispatch refusal did not reject intent")
			}
		})
	}
}

// The push gate writes upstream, so it refuses every routing or forwarding header the read
// transport does, before any bearer lookup or upstream call.
func TestRoutingHeadersRefusedBeforeAuthAndUpstream(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ name, header string }{
		{"real ip", "X-Real-Ip"}, {"via", "Via"}, {"envoy", "X-Envoy-External-Address"},
		{"underscore forwarded", "x_forwarded_for"}, {"forwarded", "X-Forwarded-For"},
		{"actor", "X-Actor-Id"}, {"session", "X-Session-Id"},
	} {
		for _, discovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s discovery=%t", tc.name, discovery), func(t *testing.T) {
				method, target := http.MethodPost, "http://localhost/oldsj/alpha.git/git-receive-pack"
				if discovery {
					method, target = http.MethodGet, "http://localhost/oldsj/alpha.git/info/refs?service=git-receive-pack"
				}
				calls, pushes := e.upstream.counts()
				r := httptest.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(nil))
				r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
				// A bearer that cannot authenticate: reaching the lookup would answer authentication_required.
				r.Header.Set("Authorization", "Bearer not-a-real-token")
				r.Header[tc.header] = []string{"10.0.0.1"}
				w := httptest.NewRecorder()
				e.server.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Body.String(), "routing_header ") {
					t.Fatalf("got %d %q", w.Code, w.Body.String())
				}
				if c, p := e.upstream.counts(); c != calls || p != pushes {
					t.Fatal("refused request reached upstream")
				}
			})
		}
	}
}

func TestAuthorizedProbeAndInvalidNeighbours(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"exact", []byte("0000"), true}, {"empty", nil, false}, {"truncated", []byte("000"), false}, {"extra byte", []byte("0000x"), false},
		{"shallow only", append(pkt([]byte("shallow "+strings.Repeat("a", 40)+"\n")), []byte("0000")...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before, _ := e.upstream.counts()
			requests := len(e.fake.Requests())
			w := e.request(tc.body, nil)
			after, _ := e.upstream.counts()
			if tc.ok {
				if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/x-git-receive-pack-result" {
					t.Fatalf("probe: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code == 200 {
				t.Fatal("invalid probe accepted")
			}
			if e.publicationCount() != 0 || after != before || requests != len(e.fake.Requests()) {
				t.Fatal("probe contacted upstream or recorded publication")
			}
			if !regexpID(w.Header().Get("x-mainloop-request-id")) {
				t.Fatal("correlation id absent")
			}
			admissionClean(t, e)
			if tc.ok {
				if _, err := os.Stat(e.server.SpoolRoot); !os.IsNotExist(err) {
					t.Fatal("exact probe allocated a spool root")
				}
			}
		})
	}
}
func regexpID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, b := range s {
		if !strings.ContainsRune("0123456789abcdef", b) {
			return false
		}
	}
	return true
}

func TestProbeRechecksAuthorityAndFence(t *testing.T) {
	for _, kind := range []string{"revoked", "policy", "writer", "runtime", "fence", "late writer fenced", "late revocation"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			switch kind {
			case "revoked":
				if _, err := e.creds.Revoke(context.Background(), "ws"); err != nil {
					t.Fatal(err)
				}
			case "policy":
				e.exec(`UPDATE git_branch_policies SET policy=policy || '{"protected_patterns":["feature/*"]}'`)
			case "writer":
				e.exec(`UPDATE workspace_writer_claims SET held=false`)
				e.exec(`UPDATE workspace_writer_claims SET held=true,generation=2`)
			case "runtime":
				e.native.session.RuntimeAssociation.CurrentActive = false
			case "fence":
				e.exec(`INSERT INTO git_publications (id,workspace_id,request_id,owner_id,repository,branch,attempt,state) VALUES ('held','ws','held','owner','oldsj/alpha','feature/test','{}','unknown')`)
			case "late writer fenced", "late revocation":
				count := 0
				e.native.before = func() {
					count++
					if count == 2 {
						if kind == "late writer fenced" {
							if _, err := e.pool.Exec(context.Background(), `UPDATE workspace_writer_claims SET held=false`); err == nil {
								t.Fatal("admitted probe allowed writer release")
							}
						} else if _, err := e.creds.Revoke(context.Background(), "ws"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			before, _ := e.upstream.counts()
			w := e.request([]byte("0000"), nil)
			after, _ := e.upstream.counts()
			if (w.Code == 200) != (kind == "late writer fenced") || after != before {
				t.Fatalf("probe authorization: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func (e *env) resolve() {
	e.t.Helper()
	e.exec(`UPDATE git_publications SET lease_until=now()-interval '1 second',next_attempt_at=now()-interval '1 second'`)
	claims, err := e.svc.ResolverLoop().Table.Claim(context.Background(), e.pool, "resolver", time.Minute, 10)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(claims) != 1 {
		e.t.Fatalf("claims = %d", len(claims))
	}
	out, err := e.svc.resolve(context.Background(), claims[0])
	if err != nil {
		e.t.Fatal(err)
	}
	// Simulate the runner's handling of a retry without peeking into Outcome's private fields.
	if e.state() == dbstate.PublicationUnknown {
		if err = claims[0].RetryAfter(context.Background(), e.pool, time.Minute); err != nil {
			e.t.Fatal(err)
		}
	}
	_ = out
}

func TestUnknownResolverObservesWithoutResend(t *testing.T) {
	for _, kind := range []string{"accepted lost reply", "not accepted", "diverged", "abandoned pending", "abandoned dispatching"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			e.upstream.lost = true
			if kind != "accepted lost reply" {
				e.upstream.reply = []byte("invalid receipt")
			}
			if strings.HasPrefix(kind, "abandoned") {
				c, err := e.svc.authenticate(context.Background(), "Bearer "+e.token)
				if err != nil {
					t.Fatal(err)
				}
				a, err := e.svc.check(context.Background(), c, "")
				if err != nil {
					t.Fatal(err)
				}
				claim, err := e.svc.admit(context.Background(), c, a, "dead-handler", e.server.Limits.defaults())
				if err != nil {
					t.Fatal(err)
				}
				defer e.svc.releaseAdmission(context.Background(), claim)
				release, err := reserveScratch(e.server.Limits.defaults())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				sp, err := newSpool(context.Background(), e.server.SpoolRoot, bytes.NewReader(e.body(zeroOID, e.head, "refs/heads/feature/test")), e.server.Limits.defaults())
				if err != nil {
					t.Fatal(err)
				}
				defer sp.close()
				cmd, err := receiveCommands(sp.body, 256<<10)
				if err != nil {
					t.Fatal(err)
				}
				row, err := e.svc.reserve(context.Background(), c, a, cmd, sp, "dead-handler")
				if err != nil {
					t.Fatal(err)
				}
				if kind == "abandoned dispatching" {
					if _, err = dbq.New(e.pool).DispatchPushPublication(context.Background(), dbq.DispatchPushPublicationParams{ID: row.ID, LeaseOwner: ptr("dead-handler")}); err != nil {
						t.Fatal(err)
					}
				}
				// A crashed handler cannot retain live admission when the abandoned intent
				// is eligible for resolution. Simulate both leases expiring independently.
				e.exec(`UPDATE git_push_admissions SET lease_until=now()-interval '1 second'`)
			} else {
				w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
				if w.Code != 503 || e.state() != dbstate.PublicationUnknown {
					t.Fatalf("unknown: %d %s", w.Code, w.Body.String())
				}
			}
			if kind == "diverged" {
				e.git(e.work, "push", e.bare, "main:feature/test")
			}
			if _, err := e.pool.Exec(context.Background(), `UPDATE workspace_writer_claims SET held=false`); err == nil {
				t.Fatal("unresolved publication released writer")
			}
			probe := e.request([]byte("0000"), nil)
			if probe.Code == 200 {
				t.Fatal("unresolved publication allowed probe")
			}
			_, before := e.upstream.counts()
			e.resolve()
			_, after := e.upstream.counts()
			if before != after {
				t.Fatal("resolver resent publication")
			}
			want := dbstate.PublicationRejected
			if kind == "accepted lost reply" {
				want = dbstate.PublicationConfirmed
			}
			if kind == "diverged" {
				want = dbstate.PublicationUnknown
			}
			if e.state() != want {
				t.Fatalf("resolved state %s, want %s", e.state(), want)
			}
			if want != dbstate.PublicationUnknown {
				e.exec(`UPDATE workspace_writer_claims SET held=false`)
			}
		})
	}
}

func TestNoDatabaseTransactionDuringNetworkIO(t *testing.T) {
	e := newEnv(t)
	// A single pool connection means an external adapter cannot query if a transaction is held.
	e.pool.Config() // configuration is copied, so build a separate one-connection pool
	cfg := e.pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	single, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	e.svc.Pool = single
	e.upstream.before = func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var n int
		if err := single.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			t.Fatal("connection held across upstream I/O")
		}
	}
	e.native.before = func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var n int
		if err := single.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			t.Fatal("connection held across native observation")
		}
	}
	body := e.body(zeroOID, e.head, "refs/heads/feature/test")
	w := e.request(body, func(r *http.Request) {
		source := bytes.NewReader(body)
		r.Body = io.NopCloser(readerFunc(func(b []byte) (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var n int
			if err := single.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
				t.Fatal("connection held across receive body I/O")
			}
			return source.Read(b)
		}))
	})
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
}

func TestClosedRefusalLogs(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	r := httptest.NewRequestWithContext(context.Background(), "POST", "http://evil.example.test/secret-path?secret=value", nil)
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("x-mainloop-request-id", "client-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if strings.Contains(logs.String(), "secret") || !strings.Contains(logs.String(), "git_transport_disabled") {
		t.Fatal("unsafe refusal log")
	}
	logs.Reset()
	w = httptest.NewRecorder()
	s.fail(w, r, "01234567890123456789012345678901", "secret-stage", denial("secret-code"))
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["code"] != "transport_unavailable" || entry["stage"] != "unknown" || strings.Contains(logs.String(), "secret") {
		t.Fatal("open denial vocabulary")
	}
}

func TestQuarantineResourceLimitsAndShallow(t *testing.T) {
	for _, kind := range []string{"objects", "expansion", "trailing", "shallow", "malformed shallow", "tag"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			b := e.body(zeroOID, e.head, "refs/heads/feature/test")
			switch kind {
			case "objects":
				e.server.Limits.Objects = 1
			case "expansion":
				e.server.Limits.ExpandedBytes = 1
			case "trailing":
				b = append(b, 1)
			case "shallow":
				b = append(pkt([]byte("shallow "+e.base+"\n")), b...)
			case "malformed shallow":
				b = append(pkt([]byte("shallow "+zeroOID+"\n")), b...)
			case "tag":
				b = e.body(zeroOID, e.head, "refs/tags/feature")
			}
			w := e.request(b, nil)
			_, pushes := e.upstream.counts()
			if kind == "shallow" {
				if w.Code != 200 || pushes != 1 {
					t.Fatalf("shallow: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code == 200 || pushes != 0 {
				t.Fatal("bad pack/ref published")
			}
		})
	}
}

func TestWriterFenceAndConcurrentPushes(t *testing.T) {
	e := newEnv(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	e.upstream.before = func(stage string) {
		if stage == "push" {
			close(entered)
			<-release
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil) }()
	<-entered
	if _, err := e.pool.Exec(context.Background(), `UPDATE workspace_writer_claims SET held=false`); err == nil {
		close(release)
		t.Fatal("in-flight writer released")
	}
	w := e.request([]byte("0000"), nil)
	if w.Code == 200 {
		close(release)
		t.Fatal("in-flight probe accepted")
	}
	w = e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
	if w.Code == 200 {
		close(release)
		t.Fatal("second in-flight publication accepted")
	}
	close(release)
	result := <-done
	if result.Code != 200 {
		t.Fatalf("first publication: %d %s", result.Code, result.Body.String())
	}
	e.exec(`UPDATE workspace_writer_claims SET held=false`)
	_, pushes := e.upstream.counts()
	if pushes != 1 {
		t.Fatal("two upstream publications")
	}
}

func TestDeletingWorkspaceRetainsPublication(t *testing.T) {
	e := newEnv(t)
	w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	e.exec(`DELETE FROM sessions WHERE id='ws'`)
	if e.publicationCount() != 1 {
		t.Fatal("lost audit record")
	}
}

func TestDelegatedWriterGenerationAndCurrentAttempt(t *testing.T) {
	for _, kind := range []string{"active", "stale attempt generation", "released claim", "not current", "draining", "completed task"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnvForRole(t, dbstate.RoleSupervisor)
			switch kind {
			case "stale attempt generation":
				e.exec(`UPDATE task_attempts SET writer_generation=1`)
			case "released claim":
				e.exec(`UPDATE workspace_writer_claims SET held=false`)
			case "not current":
				e.exec(`UPDATE tasks SET current_attempt_id=NULL`)
			case "draining":
				e.exec(`UPDATE task_attempts SET state='draining',stop_state='requested'`)
			case "completed task":
				e.exec(`UPDATE tasks SET status='completed'`)
			}
			w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
			_, pushes := e.upstream.counts()
			if kind == "active" {
				if w.Code != 200 || pushes != 1 {
					t.Fatalf("delegated writer: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code == 200 || pushes != 0 {
				t.Fatal("stale delegated writer published")
			}
		})
	}
}

func TestPolicyVersionsAndPreviousDefaults(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SetPolicy(context.Background(), "p", 1, Policy{DefaultBranch: "trunk", ProtectedPatterns: []string{"release/*"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetPolicy(context.Background(), "p", 1, Policy{DefaultBranch: "x"}); err == nil {
		t.Fatal("stale policy version accepted")
	}
	if err := e.svc.SetPolicy(context.Background(), "p", 2, Policy{DefaultBranch: "stable"}); err != nil {
		t.Fatal(err)
	}
	row, err := dbq.New(e.pool).GetPushPolicy(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	var p Policy
	if err = json.Unmarshal(row.Policy, &p); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"main", "trunk", "stable"} {
		if p.permits(b) == nil {
			t.Fatal("default branch released")
		}
	}
	for _, pattern := range []string{"*", "feature/*", "feature/[!x]*"} {
		if (Policy{DefaultBranch: "main", ProtectedPatterns: []string{pattern}}).permits("feature/test/nested") == nil {
			t.Fatal("protected wildcard did not cover nested branch")
		}
	}
	if (Policy{DefaultBranch: "main", AllowedPatterns: []string{"feature/*"}}).permits("feature/test/nested") != nil {
		t.Fatal("allowed wildcard")
	}
}

// The release branch carries what Argo deploys to production, so no policy can open it to tasks.
func TestReleaseBranchIsAlwaysDenied(t *testing.T) {
	for _, p := range []Policy{
		{DefaultBranch: "main"},
		{DefaultBranch: "main", AllowedPatterns: []string{"release", "*"}},
		{DefaultBranch: "trunk"},
	} {
		if p.permits("release") == nil || p.Allows("release") {
			t.Fatalf("release permitted under %+v", p)
		}
	}
	if (Policy{DefaultBranch: "main"}).permits("release-notes") != nil {
		t.Fatal("only the exact release branch is denied")
	}
}

func TestBranchGlobClasses(t *testing.T) {
	for _, tc := range []struct {
		pattern, branch string
		want            bool
	}{
		{"feature/[^a]*", "feature/abc", true}, // fnmatch '^' is literal, not class negation
		{"feature/[^a]*", "feature/test", false},
		{"feature/[!a]*", "feature/abc", false},
		{"feature/[!a]*", "feature/test/nested", true},
		{"feature/[a-z]?", "feature/az", true},
		{"feature/[a-z]?", "feature/1z", false},
		{"feature/*", "feature/one/two", true},
	} {
		got, err := matchBranch(tc.pattern, tc.branch)
		if err != nil || got != tc.want {
			t.Fatalf("glob %s: %v, %v", tc.pattern, got, err)
		}
	}
}

func TestChildRequiresLiveParent(t *testing.T) {
	for _, kind := range []string{"active", "archived", "lost claim", "completed", "not current"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnvForRole(t, dbstate.RoleChild)
			switch kind {
			case "archived":
				e.exec(`UPDATE sessions SET archived_at=now() WHERE id='parent-ws'`)
			case "lost claim":
				e.exec(`UPDATE workspace_writer_claims SET held=false WHERE attempt_id='parent-attempt'`)
			case "completed":
				e.exec(`UPDATE tasks SET status='completed' WHERE id='parent-task'`)
			case "not current":
				e.exec(`UPDATE tasks SET current_attempt_id=NULL WHERE id='parent-task'`)
			}
			w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
			_, pushes := e.upstream.counts()
			if kind == "active" {
				if w.Code != 200 || pushes != 1 {
					t.Fatalf("child writer: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code == 200 || pushes != 0 {
				t.Fatal("child of inactive parent published")
			}
		})
	}
}

func TestFreshAuthorityBeforePublication(t *testing.T) {
	for _, kind := range []string{"writer", "runtime", "policy", "remote ref"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			e.upstream.before = func(stage string) {
				if stage != "metadata" {
					return
				}
				switch kind {
				case "writer":
					e.exec(`UPDATE task_attempts SET state=state`)
					e.exec(`UPDATE agent_bindings SET mcp_grant_kind='none' WHERE session_id='ws'`)
				case "runtime":
					e.native.mu.Lock()
					e.native.session.RuntimeAssociation.ActorUid = "replacement"
					e.native.mu.Unlock()
				case "policy":
					if err := e.svc.SetPolicy(context.Background(), "p", 1, Policy{DefaultBranch: "main", ProtectedPatterns: []string{"*"}}); err != nil {
						t.Fatal(err)
					}
				case "remote ref":
					e.git(e.work, "push", e.bare, "main:feature/test")
				}
			}
			w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
			_, pushes := e.upstream.counts()
			if w.Code == 200 || pushes != 0 || e.state() != dbstate.PublicationRejected {
				t.Fatalf("authority after validation: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPublicationIdentityAndTerminalState(t *testing.T) {
	e := newEnv(t)
	w := e.request(e.body(zeroOID, e.head, "refs/heads/feature/test"), nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, query := range []string{`UPDATE git_publications SET attempt='{}'`, `UPDATE git_publications SET state='pending'`, `UPDATE git_push_grants SET writer_generation=2`} {
		if _, err := e.pool.Exec(context.Background(), query); err == nil {
			t.Fatal("durable authority mutated")
		}
	}
}
