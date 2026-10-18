package workspaces

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// This upstream uses real Git and a bare repository; no GitHub or live native agent is contacted.
type localPushUpstream struct {
	bare     string
	metadata int
	before   func(context.Context) error
}

func (u *localPushUpstream) DefaultBranch(ctx context.Context, _ github.Repo) (string, error) {
	if u.before != nil {
		if err := u.before(ctx); err != nil {
			return "", err
		}
	}
	u.metadata++
	return "main", nil
}

func (u *localPushUpstream) Refs(ctx context.Context, _ github.Repo) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "git", "--git-dir="+u.bare, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs, err
}

func (u *localPushUpstream) Seed(ctx context.Context, _ github.Repo, wants []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+u.bare, "pack-objects", "--stdout", "--revs")
	cmd.Stdin = strings.NewReader(strings.Join(wants, "\n") + "\n")
	cmd.Stdout = w
	return cmd.Run()
}

func (u *localPushUpstream) Push(ctx context.Context, _ github.Repo, body io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "receive-pack", "--stateless-rpc", u.bare)
	cmd.Stdin = body
	return cmd.Output()
}

// withGitProxies configures what a workspace with a development environment needs: the git read
// transport and an enabled push gate. kagent prepares a Session only if it holds a reference to each.
func (h *harness) withGitProxies() *authtest.Env {
	h.t.Helper()
	a := h.withGitRead()
	u := &localPushUpstream{}
	h.svc.GitPush = &GitPush{Gate: pushgate.NewService(h.svc.Pool, h.svc.Kagent, u, a.Creds), Origin: h.cfg.Git.PushOrigin}
	return a
}

func queuedPushWorkspace(t *testing.T, h *harness, branch string, task bool) (wsRow, *authtest.Env, *localPushUpstream) {
	t.Helper()
	return queuedPushWorkspaceWithDefault(t, h, branch, task, "main")
}

// queuedPushWorkspaceWithDefault queues the workspace of a project whose stored default branch is
// def; empty is a project imported without GitHub metadata.
func queuedPushWorkspaceWithDefault(t *testing.T, h *harness, branch string, task bool, def string) (wsRow, *authtest.Env, *localPushUpstream) {
	t.Helper()
	a := h.withGitRead()
	u := &localPushUpstream{}
	h.svc.GitPush = &GitPush{Gate: pushgate.NewService(h.svc.Pool, h.svc.Kagent, u, a.Creds), Origin: h.cfg.Git.PushOrigin}
	h.cfg.Workspaces.OperationWait = 10e6
	w, err := h.create(api.CreateWorkspaceRequest{Branch: &branch})
	if err != nil {
		t.Fatal(err)
	}
	r := h.row(w.WorkspaceId)
	h.exec(`UPDATE projects SET default_branch=$2 WHERE id=$1`, r.ProjectID, def)
	h.exec(`UPDATE workspaces SET environment=$2 WHERE id=$1`, r.ID, testEnvironment)
	if task {
		h.exec(`INSERT INTO tasks (id,owner_id,project_id,root_task_id,title,brief,mode,assigned_profile_id,selection_source,status,checkout_branch,checkout_ref,checkout_depth)
		    VALUES ('task',$1,$2,'task','test','test','code','claude','explicit','queued',$3,'main',0)`, owner, r.ProjectID, r.Branch)
		h.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,workspace_request_id,writer_generation)
		    VALUES ('attempt','task',1,'claude','claude','rev',$1,$2,'supervisor',1,$3,1)`, r.AgentNamespace, r.AgentName, r.RequestID)
		h.exec(`UPDATE tasks SET current_attempt_id='attempt' WHERE id='task'`)
		h.exec(`INSERT INTO workspace_writer_claims (owner_id,repository,branch,generation,attempt_id)
		    VALUES ($1,'oldsj/mainloop',$2,1,'attempt')`, owner, r.Branch)
	}
	r = h.row(r.ID)
	ref := pushgate.Reference(a.Creds, h.cfg.Git.PushOrigin, r.ID)
	// The shared kagent fake omits credentials and runtime attestation in its Session output.
	// Supply those output-only facts after checking the real CreateSession request below.
	baseTamper := h.fake.TamperSession
	h.fake.TamperSession = func(proc string, n *ka.Session) {
		baseTamper(proc, n)
		if proc == kagent.ProcGetSession {
			n.Operation = ka.RuntimeOperation_RUNTIME_OPERATION_NONE
			n.Credentials = []*ka.SessionCredential{proto.CloneOf(ref)}
			n.PreparedRevision = "revision"
			n.RuntimeComposition = &ka.RuntimeComposition{PayloadImage: "example.test/harness@sha256:def", Provider: "claude", Schema: 1}
			n.RuntimeAssociation = &ka.RuntimeAssociation{GenerationId: "generation", ActorUid: "actor-uid", ActorName: "actor", Atespace: "space", Phase: "active", CurrentActive: true}
			if v := h.getState.Load(); v != 0 {
				n.State = ka.RuntimeState(v)
			}
		}
	}
	return r, a, u
}

func createPushSession(t *testing.T, h *harness) {
	t.Helper()
	claims, err := Table().Claim(bg, h.svc.Pool, "create-test", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("create claim: %v, %v", claims, err)
	}
	if _, err = h.svc.handle(bg, claims[0]); err != nil {
		t.Fatal(err)
	}
}

func activatePushAttempt(h *harness, id string) {
	h.exec(`UPDATE task_attempts SET state='active',workspace_id=$1,session_id=$1,binding_id=$1 WHERE id='attempt'`, id)
	h.exec(`UPDATE tasks SET status='running' WHERE id='task'`)
	h.exec(`UPDATE agent_bindings SET role='supervisor' WHERE session_id=$1`, id)
}

func reconcilePush(t *testing.T, h *harness, id string) error {
	t.Helper()
	claims, err := PushReservationTable().Claim(bg, h.svc.Pool, "push-test", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("push claim: %v, %v", claims, err)
	}
	_, err = h.svc.enrollPush(bg, claims[0])
	r, readErr := h.dq.GetWorkspacePushReservation(bg, id)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if r.State == string(dbstate.GitGrantPlanned) {
		if retryErr := claims[0].RetryAfter(bg, h.svc.Pool, 0); retryErr != nil {
			t.Fatal(retryErr)
		}
	}
	return err
}

func pushCredentials(h *harness, id string) int {
	n := 0
	for _, c := range h.credentialRows(id) {
		if c.Kind == dbstate.CredentialGitPush {
			n++
		}
	}
	return n
}

func TestWorkspacePushAttachmentAndReadiness(t *testing.T) {
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
	createPushSession(t, h)
	reqs := h.fake.CreateRequests()
	want := pushgate.Reference(a.Creds, h.cfg.Git.PushOrigin, w.ID)
	attached := 0
	for _, c := range reqs[0].GetCredentials() {
		if proto.Equal(c, want) {
			attached++
		}
	}
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.IdempotencyKey != "workspace-push:attempt" || attached != 1 || pushCredentials(h, w.ID) != 0 {
		t.Fatalf("attachment/reservation: refs=%d state=%s error=%v", attached, r.State, err)
	}
	// Native readiness alone is insufficient until the task caller activates its writer.
	if err = reconcilePush(t, h, w.ID); err == nil || u.metadata != 0 || pushCredentials(h, w.ID) != 0 {
		t.Fatal("enrollment ran before task authority was active")
	}
	activatePushAttempt(h, w.ID)
	h.getState.Store(int32(ka.RuntimeState_RUNTIME_STATE_CREATING))
	if err = reconcilePush(t, h, w.ID); err != nil || u.metadata != 0 || pushCredentials(h, w.ID) != 0 {
		t.Fatal("enrollment ran before native readiness")
	}
	h.getState.Store(0)
	if err = reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	r, err = h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.State != string(dbstate.GitGrantPublished) || pushCredentials(h, w.ID) != 1 {
		t.Fatal("ready writer was not enrolled")
	}
	if a.Cluster.UID(authtest.Namespace, want.GetSecretRef().GetName()) == "" {
		t.Fatal("push Secret was not published")
	}
	// The lifecycle reports the authority the workspace actually holds once the gate enrolls it.
	if lc, err := h.svc.Get(bg, w.ID); err != nil || lc.PublicationMode != api.Branch || lc.PublicationReason != nil {
		t.Fatalf("lifecycle publication after enrollment: %v, mode %v, reason %v", err, lc.PublicationMode, lc.PublicationReason)
	}
	// Another workspace cannot take this attempt's key, and terminal release cannot reopen it.
	if _, err = h.svc.Pool.Exec(bg, `INSERT INTO workspace_push_reservations (id,idempotency_key,workspace_id,attempt_id,reference,expires_at)
	    VALUES ('other','workspace-push:attempt','other','attempt','{}',now()+interval '1 hour')`); err == nil {
		t.Fatal("attempt reserved twice")
	}
}

func TestWorkspacePushReplayReusesReference(t *testing.T) {
	h := newHarness(t, nil)
	w, _, _ := queuedPushWorkspace(t, h, "feature/test", true)
	tamper := h.fake.TamperSession
	first := true
	h.fake.TamperSession = func(proc string, n *ka.Session) {
		tamper(proc, n)
		if proc == kagent.ProcCreateSession && first {
			first = false
			n.Id = ""
		}
	}
	createPushSession(t, h)
	h.exec(`UPDATE workspace_ops SET next_attempt_at=now() WHERE workspace_id=$1`, w.ID)
	// Removing current configuration cannot change the already-dispatched contract.
	h.svc.GitPush = nil
	createPushSession(t, h)
	reqs := h.fake.CreateRequests()
	if len(reqs) != 2 || !credentialsEqual(reqs[0].Credentials, reqs[1].Credentials) || len(h.sessions()) != 1 || pushCredentials(h, w.ID) != 0 {
		t.Fatalf("replay changed the reference or minted a credential/Session: requests=%d sessions=%d push credentials=%d", len(reqs), len(h.sessions()), pushCredentials(h, w.ID))
	}
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.AttemptID != "attempt" {
		t.Fatal("replay lost the original reservation")
	}
}

func TestWorkspacePushFailureAndExpiryRelease(t *testing.T) {
	for _, kind := range []string{"refused", "expired", "failed runtime", "abandoned task", "no environment"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			w, _, _ := queuedPushWorkspace(t, h, "feature/test", true)
			if kind == "refused" {
				h.fake.FailNext(kagent.ProcCreateSession, grpcFault(connect.CodeInvalidArgument))
			}
			createPushSession(t, h)
			switch kind {
			case "expired":
				h.exec(`UPDATE workspace_push_reservations SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, w.ID)
			case "failed runtime":
				h.getState.Store(int32(ka.RuntimeState_RUNTIME_STATE_FAILED))
			case "abandoned task":
				h.exec(`UPDATE task_attempts SET state='draining',stop_state='requested' WHERE id='attempt'`)
			case "no environment":
				// The task is live and the session ready: only the missing environment ends the wait.
				activatePushAttempt(h, w.ID)
				h.exec(`UPDATE workspaces SET environment=NULL WHERE id=$1`, w.ID)
			}
			if kind != "refused" {
				if err := reconcilePush(t, h, w.ID); err != nil {
					t.Fatal(err)
				}
			}
			r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
			if err != nil || r.State != string(dbstate.GitGrantRevoked) || pushCredentials(h, w.ID) != 0 {
				t.Fatal("creation reservation not released")
			}
			// Release preserves the original reference for native lookup/cleanup replay.
			ref, err := frozenReference(h.row(w.ID).GitPushCredential)
			if err != nil || ref == nil {
				t.Fatal("release discarded the native replay contract")
			}
		})
	}
}

// A workspace with an environment that cannot push still holds the push reference kagent needs to
// prepare it, but no reservation, grant or Secret ever backs that reference.
func TestWorkspacePushReadOnlyAndExistingSessionGetNoAuthority(t *testing.T) {
	for _, kind := range []string{"standalone", "default branch", "protected branch", "lost writer", "existing session"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			branch := "feature/test"
			if kind == "default branch" {
				branch = "main"
			}
			w, _, _ := queuedPushWorkspace(t, h, branch, kind != "standalone")
			if kind == "protected branch" {
				h.exec(`INSERT INTO git_branch_policies (project_id,policy) VALUES ($1,'{"default_branch":"main","protected_patterns":["feature/*"]}')`, w.ProjectID)
			}
			if kind == "lost writer" {
				h.exec(`UPDATE workspace_writer_claims SET held=false WHERE attempt_id='attempt'`)
			}
			if kind == "existing session" {
				// The Session was made before the decision existed (a row from before the freeze): deciding
				// now must not retrofit a reference or reserve anything.
				createPushSession(t, h)
				h.exec(`DELETE FROM workspace_push_reservations WHERE workspace_id=$1`, w.ID)
				h.exec(`UPDATE workspaces SET git_push_decided=false,git_push_credential=NULL WHERE id=$1`, w.ID)
				ref, err := h.svc.gitPushForCreate(bg, h.row(w.ID))
				if err != nil || ref != nil {
					t.Fatal("existing Session was retrofitted")
				}
			} else {
				createPushSession(t, h)
			}
			if _, err := h.dq.GetWorkspacePushReservation(bg, w.ID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("read-only workspace got a reservation")
			}
			if pushCredentials(h, w.ID) != 0 || len(h.fake.CreateRequests()[0].Credentials) != 3 {
				t.Fatal("read-only workspace got push authority or no push reference")
			}
		})
	}
}

// pushFixture is a work tree on the workspace's branch and a bare upstream behind u, both under a
// cache directory removed at the end of the test.
type pushFixture struct {
	t    *testing.T
	root string
	work string
	head string
	git  func(dir string, args ...string) string
}

func newPushFixture(t *testing.T, h *harness, w wsRow, u *localPushUpstream) *pushFixture {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(cache, "mainloop-workspace-push-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	u.bare = filepath.Join(root, "upstream.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(bg, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("local git fixture failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, dir := range []string{work, u.bare} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	git(u.bare, "init", "--bare", "-b", "main")
	git(work, "init", "-b", "main")
	git(work, "commit", "--allow-empty", "-m", "base")
	base := git(work, "rev-parse", "HEAD")
	git(work, "push", u.bare, "main")
	git(work, "checkout", "-b", w.Branch)
	git(work, "commit", "--allow-empty", "-m", "workspace change")
	h.exec(`UPDATE workspaces SET ref=$1 WHERE id=$2`, base, w.ID)
	return &pushFixture{t: t, root: root, work: work, head: git(work, "rev-parse", "HEAD"), git: git}
}

// pushThroughGate pushes the workspace branch to a real gate with the workspace-held credential
// and checks the upstream received the workspace head.
func (f *pushFixture) pushThroughGate(h *harness, a *authtest.Env, w wsRow, u *localPushUpstream) {
	f.t.Helper()
	t := f.t
	ref := pushgate.Reference(a.Creds, h.cfg.Git.PushOrigin, w.ID)
	secret, err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Get(bg, ref.GetSecretRef().GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	credential := secret.Data[ref.GetSecretRef().GetKey()]
	gate := pushgate.NewService(h.svc.Pool, h.client, u, nil)
	server := &pushgate.Server{Service: gate, Enabled: true, Dev: true, SpoolRoot: filepath.Join(f.root, "spool"), Limits: pushgate.Limits{BodyBytes: 1 << 20, DiskBytes: 8 << 20}}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	// Git reads the credential from the fake workspace-held Secret. Do not put it in argv/logs.
	cmd := exec.CommandContext(bg, "git", "-C", f.work, "push", proxy.URL+"/oldsj/mainloop.git", w.Branch)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + f.root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: " + string(credential), "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace push failed: %v", err)
	}
	if bytes.Contains(out, credential) {
		t.Fatal("git exposed the workspace credential")
	}
	if got := f.git(u.bare, "rev-parse", "refs/heads/"+w.Branch); got != f.head {
		t.Fatal("gate did not publish the workspace head")
	}
}

func TestWorkspaceHeldCredentialPushesThroughGate(t *testing.T) {
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
	f := newPushFixture(t, h, w, u)
	createPushSession(t, h)
	activatePushAttempt(h, w.ID)
	if err := reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	f.pushThroughGate(h, a, w, u)
	if pushCredentials(h, w.ID) != 1 {
		t.Fatal("push created another credential")
	}
}

func TestWorkspacePushDeletionReplayAndRelease(t *testing.T) {
	h := newHarness(t, nil)
	w, _, _ := queuedPushWorkspace(t, h, "feature/test", true)
	tamper := h.fake.TamperSession
	first := true
	h.fake.TamperSession = func(proc string, n *ka.Session) {
		tamper(proc, n)
		if proc == kagent.ProcCreateSession && first {
			first = false
			n.Id = ""
		}
	}
	createPushSession(t, h)
	h.svc.GitPush = nil
	h.cfg.Workspaces.OperationWait = 5e9
	stop := h.run()
	defer stop()
	if err := h.svc.Delete(bg, w.ID); err != nil {
		t.Fatal(err)
	}
	reqs := h.fake.CreateRequests()
	if len(reqs) != 2 || !credentialsEqual(reqs[0].Credentials, reqs[1].Credentials) || len(h.sessions()) != 0 {
		t.Fatal("delete did not replay and remove the original Session")
	}
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.State != string(dbstate.GitGrantRevoked) || pushCredentials(h, w.ID) != 0 {
		t.Fatal("delete leaked push authority")
	}
}

func TestWorkspacePushEnrollmentRequiresOriginalAttachment(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "busy"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			w, _, _ := queuedPushWorkspace(t, h, "feature/test", true)
			createPushSession(t, h)
			activatePushAttempt(h, w.ID)
			tamper := h.fake.TamperSession
			h.fake.TamperSession = func(proc string, n *ka.Session) {
				tamper(proc, n)
				if proc == kagent.ProcGetSession {
					switch kind {
					case "missing":
						n.Credentials = nil
					case "duplicate":
						n.Credentials = append(n.Credentials, proto.CloneOf(n.Credentials[0]))
					case "busy":
						n.Operation = ka.RuntimeOperation_RUNTIME_OPERATION_RESUME
					}
				}
			}
			if err := reconcilePush(t, h, w.ID); err != nil {
				t.Fatal(err)
			}
			r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
			want := dbstate.GitGrantRevoked
			if kind == "busy" {
				want = dbstate.GitGrantPlanned
			}
			if err != nil || r.State != string(want) || pushCredentials(h, w.ID) != 0 {
				t.Fatal("enrolled an unattached or busy Session")
			}
		})
	}
}

type checkedPushNative struct {
	Kagent
	check func(context.Context) error
}

func (n checkedPushNative) CreateSession(ctx context.Context, req *ka.CreateSessionRequest) (*ka.Session, error) {
	if err := n.check(ctx); err != nil {
		return nil, err
	}
	return n.Kagent.CreateSession(ctx, req)
}

func (n checkedPushNative) GetSession(ctx context.Context, id string) (*ka.Session, error) {
	if err := n.check(ctx); err != nil {
		return nil, err
	}
	return n.Kagent.GetSession(ctx, id)
}

type checkedPushSecrets struct {
	agentauth.SecretStore
	check func(context.Context) error
}

func (s checkedPushSecrets) Create(ctx context.Context, name, purpose, bearer string) (string, error) {
	if err := s.check(ctx); err != nil {
		return "", err
	}
	return s.SecretStore.Create(ctx, name, purpose, bearer)
}

func TestWorkspacePushReservationBeforeRPCAndNoTransactionAcrossIO(t *testing.T) {
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
	cfg := h.svc.Pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	single, err := pgxpool.NewWithConfig(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	h.svc.Pool, a.Creds.Pool = single, single
	check := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var n int
		return single.QueryRow(ctx, `SELECT 1`).Scan(&n)
	}
	h.svc.Kagent = checkedPushNative{Kagent: h.client, check: func(ctx context.Context) error {
		if err := check(ctx); err != nil {
			return err
		}
		var reserved bool
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		err := single.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_push_reservations WHERE workspace_id=$1 AND idempotency_key='workspace-push:attempt')`, w.ID).Scan(&reserved)
		if err == nil && !reserved {
			return errors.New("native RPC preceded its durable push reservation")
		}
		return err
	}}
	a.Creds.Secrets = checkedPushSecrets{SecretStore: a.Creds.Secrets, check: check}
	u.before = check
	createPushSession(t, h)
	if h.row(w.ID).KagentSessionID == nil {
		t.Fatal("creation retained a connection across native or Secret I/O")
	}
	activatePushAttempt(h, w.ID)
	if err = reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	if pushCredentials(h, w.ID) != 1 {
		t.Fatal("enrollment retained a connection across native, metadata or Secret I/O")
	}
}

// kagent parks an idle Actor and attests the runtime only while one runs, so enrollment wakes the
// Session and waits for the Actor instead of looping on an unattested runtime.
func TestWorkspacePushEnrollmentWakesAnIdleActor(t *testing.T) {
	h := newHarness(t, nil)
	w, _, u := queuedPushWorkspace(t, h, "feature/test", true)
	createPushSession(t, h)
	activatePushAttempt(h, w.ID)
	tamper := h.fake.TamperSession
	awake := false
	h.fake.TamperSession = func(proc string, n *ka.Session) {
		tamper(proc, n)
		if proc == kagent.ProcGetSession && !awake {
			n.RuntimeAssociation = nil
		}
	}
	if err := reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	if h.fake.Count(kagent.ProcResumeSession) != 1 || u.metadata != 0 || pushCredentials(h, w.ID) != 0 {
		t.Fatalf("idle Actor: resumes=%d metadata=%d credentials=%d", h.fake.Count(kagent.ProcResumeSession), u.metadata, pushCredentials(h, w.ID))
	}
	awake = true
	if err := reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.State != string(dbstate.GitGrantPublished) || pushCredentials(h, w.ID) != 1 {
		t.Fatalf("awake Actor was not enrolled: %v %s", err, r.State)
	}
}
