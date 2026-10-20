package workspaces

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Revocation and deletion regressions for creation-time push attachment.

type reviewPushSecrets struct {
	agentauth.SecretStore
	after func(context.Context, string) error
}

func (s reviewPushSecrets) Create(ctx context.Context, name, purpose, bearer string) (string, error) {
	uid, err := s.SecretStore.Create(ctx, name, purpose, bearer)
	if err == nil && purpose == "git-push" && s.after != nil {
		err = s.after(ctx, name)
	}
	return uid, err
}

func reviewCredential(t *testing.T, h *harness, a *authtest.Env, id string) (string, string) {
	t.Helper()
	name := a.Creds.SecretName(dbstate.CredentialGitPush, id)
	secret, err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Get(bg, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal("workspace push Secret unavailable")
	}
	return string(secret.Data["authorization"]), name
}

func reviewProbe(gate *pushgate.Service, auth, repository string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(bg, http.MethodPost, "http://localhost/"+repository+".git/git-receive-pack", strings.NewReader("0000"))
	r.Header.Set("Authorization", auth)
	r.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	w := httptest.NewRecorder()
	(&pushgate.Server{Service: gate, Enabled: true, Dev: true}).Handler().ServeHTTP(w, r)
	return w
}

// runCredentialCleanup runs the real credential cleanup loop for the test's lifetime.
func runCredentialCleanup(t *testing.T, h *harness, a *authtest.Env) {
	t.Helper()
	runner := reconcile.NewRunner(h.svc.Pool, h.svc.Log, nil)
	a.Creds.Register(runner)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); _ = runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestReviewPublishedAuthorityRevocation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"archive", "delete intent", "attempt change", "generation change"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
			createPushSession(t, h)
			activatePushAttempt(h, w.ID)
			if err := reconcilePush(t, h, w.ID); err != nil {
				t.Fatal(err)
			}
			auth, name := reviewCredential(t, h, a, w.ID)
			gate := pushgate.NewService(h.svc.Pool, h.svc.Kagent, u, a.Creds)
			if res := reviewProbe(gate, auth, "oldsj/mainloop"); res.Code != http.StatusOK {
				t.Fatal("fixture never acquired push authority")
			}
			switch kind {
			case "archive":
				h.exec(`UPDATE sessions SET archived_at=now() WHERE id=$1`, w.ID)
			case "delete intent":
				if _, err := h.svc.enqueue(bg, w.ID, dbstate.OpDelete, nil); err != nil {
					t.Fatal(err)
				}
				// The intent revokes push authority itself, before any sweep: the gate's own
				// delete-op check is a snapshot read and does not serialize with a dispatch.
				if g, err := h.dq.GetPushGrant(bg, w.ID); err != nil || g.State != dbstate.GitGrantRevoked {
					t.Fatal("delete intent left the grant unrevoked")
				}
				for _, c := range h.credentialRows(w.ID) {
					if c.Kind == dbstate.CredentialGitPush && c.State != dbstate.CredentialRevoked {
						t.Fatal("delete intent left the push credential unrevoked")
					}
				}
			case "attempt change":
				h.exec(`UPDATE task_attempts SET state='fenced',stop_state='confirmed' WHERE id='attempt'`)
				h.exec(`INSERT INTO task_attempts (id,task_id,number,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,workspace_request_id)
				 SELECT 'successor',task_id,2,profile_id,native_provider,configuration_revision,agent_namespace,agent_name,role,depth,'successor-request' FROM task_attempts WHERE id='attempt'`)
				h.exec(`UPDATE tasks SET current_attempt_id='successor' WHERE id='task'`)
			case "generation change":
				h.exec(`UPDATE workspace_writer_claims SET held=false WHERE attempt_id='attempt'`)
				h.exec(`UPDATE workspace_writer_claims SET held=true,generation=generation+1 WHERE attempt_id='attempt'`)
			}
			before := reviewProbe(gate, auth, "oldsj/mainloop")
			if before.Code == http.StatusOK {
				t.Errorf("%s left published push authority usable before sweep", kind)
			}
			if err := h.svc.releaseEndedPush(bg); err != nil {
				t.Fatal(err)
			}
			r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
			if err != nil || r.State != string(dbstate.GitGrantRevoked) {
				t.Fatal("ended reservation not revoked")
			}
			g, err := h.dq.GetPushGrant(bg, w.ID)
			if err != nil || g.State != dbstate.GitGrantRevoked {
				t.Fatal("ended grant not revoked")
			}
			for _, c := range h.credentialRows(w.ID) {
				if c.Kind == dbstate.CredentialGitPush && c.State != dbstate.CredentialRevoked {
					t.Fatal("ended credential not revoked")
				}
			}
			if res := reviewProbe(gate, auth, "oldsj/mainloop"); res.Code == http.StatusOK {
				t.Fatal("ended credential usable after sweep")
			}
			runCredentialCleanup(t, h, a)
			eventually(t, "revoked push Secret deleted", func() bool { return a.Cluster.UID(authtest.Namespace, name) == "" })
			t.Logf("%s: before-sweep HTTP %d, reservation/grant/credential revoked, Secret removed", kind, before.Code)
		})
	}
}

func TestReviewRevocationDuringSecretIO(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w, a, _ := queuedPushWorkspace(t, h, "feature/test", true)
	createPushSession(t, h)
	activatePushAttempt(h, w.ID)
	called := false
	a.Creds.Secrets = reviewPushSecrets{SecretStore: a.Creds.Secrets, after: func(ctx context.Context, _ string) error {
		called = true
		h.exec(`UPDATE task_attempts SET state='draining',stop_state='requested' WHERE id='attempt'`)
		return h.svc.releaseEndedPush(ctx)
	}}
	enrollmentErr := reconcilePush(t, h, w.ID)
	if enrollmentErr == nil {
		t.Fatal("revocation during publication was ignored")
	}
	if !called {
		t.Fatalf("probe did not reach Secret I/O: %v", enrollmentErr)
	}
	r, err := h.dq.GetWorkspacePushReservation(bg, w.ID)
	if err != nil || r.State != string(dbstate.GitGrantRevoked) {
		t.Fatalf("in-flight reservation state=%s, enrollment error=%v, read error=%v", r.State, enrollmentErr, err)
	}
	if a.Cluster.UID(authtest.Namespace, a.Creds.SecretName(dbstate.CredentialGitPush, w.ID)) != "" {
		t.Fatal("in-flight revoked Secret survived")
	}
}

func TestReviewPartialGrantReleaseAndDeletion(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"expired", "failed runtime", "abandoned task", "delete"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			w, a, _ := queuedPushWorkspace(t, h, "feature/test", true)
			createPushSession(t, h)
			activatePushAttempt(h, w.ID)
			a.Creds.Secrets = reviewPushSecrets{SecretStore: a.Creds.Secrets, after: func(context.Context, string) error { return errors.New("fixture lost Secret create reply") }}
			if err := reconcilePush(t, h, w.ID); err == nil {
				t.Fatal("fixture failed to interrupt enrollment")
			}
			g, err := h.dq.GetPushGrant(bg, w.ID)
			if err != nil || g.State != dbstate.GitGrantPlanned || g.CredentialID != nil {
				t.Fatal("fixture did not leave unlinked planned grant")
			}
			name := a.Creds.SecretName(dbstate.CredentialGitPush, w.ID)
			if a.Cluster.UID(authtest.Namespace, name) == "" {
				t.Fatal("fixture left no Secret to clean up")
			}
			runCredentialCleanup(t, h, a)
			switch kind {
			case "expired":
				h.exec(`UPDATE workspace_push_reservations SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, w.ID)
			case "failed runtime":
				h.getState.Store(int32(ka.RuntimeState_RUNTIME_STATE_FAILED))
			case "abandoned task":
				h.exec(`UPDATE task_attempts SET state='draining',stop_state='requested' WHERE id='attempt'`)
			case "delete":
				if _, err = h.svc.enqueue(bg, w.ID, dbstate.OpDelete, nil); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "delete" {
				claims, claimErr := Table().Claim(bg, h.svc.Pool, "review-delete", time.Minute, 1)
				if claimErr != nil || len(claims) != 1 {
					t.Fatal("delete claim failed")
				}
				_, err = h.svc.handle(bg, claims[0])
			} else {
				err = reconcilePush(t, h, w.ID)
			}
			if err != nil {
				t.Errorf("%s cleanup failed: %v", kind, err)
			}
			r, readErr := h.dq.GetWorkspacePushReservation(bg, w.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var state string
			if queryErr := h.svc.Pool.QueryRow(bg, `SELECT state FROM credentials WHERE binding_id=$1 AND kind='git_push'`, w.ID).Scan(&state); queryErr != nil {
				t.Fatal(queryErr)
			}
			if r.State != string(dbstate.GitGrantRevoked) {
				t.Error("partial grant cleanup rolled back reservation revocation")
			}
			if state != string(dbstate.CredentialRevoked) && state != string(dbstate.CredentialDeleted) {
				t.Error("partial grant cleanup failed to revoke credential")
			}
			if kind == "delete" && (h.workspaceCount() != 0 || len(h.sessions()) != 0) {
				t.Error("confirmed native deletion could not remove workspace rows")
			}
			eventually(t, "the stale push Secret is removed", func() bool { return a.Cluster.UID(authtest.Namespace, name) == "" })
		})
	}
}

func TestReviewWorkspaceCanPushAfterDeletionIntent(t *testing.T) {
	// Not parallel: pushAfterDeletionIntent pushes through pushgate's real disk budget; see the
	// comment in TestWorkspaceHeldCredentialPushesThroughGate (gitpush_test.go).
	t.Run("delayed native delete", func(t *testing.T) { pushAfterDeletionIntent(t, false) })
	t.Run("failed native delete", func(t *testing.T) { pushAfterDeletionIntent(t, true) })
}

func pushAfterDeletionIntent(t *testing.T, nativeDeleteFails bool) {
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
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
	head := git(work, "rev-parse", "HEAD")
	h.exec(`UPDATE workspaces SET ref=$1 WHERE id=$2`, base, w.ID)
	createPushSession(t, h)
	activatePushAttempt(h, w.ID)
	if err = reconcilePush(t, h, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.enqueue(bg, w.ID, dbstate.OpDelete, nil); err != nil {
		t.Fatal(err)
	}
	live, err := h.dq.WorkspacePushReservationLive(bg, w.ID)
	if err != nil || live {
		t.Fatal("delete did not end reservation liveness")
	}
	if nativeDeleteFails {
		h.fake.FailNext(kagent.ProcDeleteSession, kagenttestDrop)
		claims, claimErr := Table().Claim(bg, h.svc.Pool, "push-delete", time.Minute, 1)
		if claimErr != nil || len(claims) != 1 {
			t.Fatal("delete claim failed")
		}
		if _, err = h.svc.handle(bg, claims[0]); err != nil {
			t.Log("native delete failed as injected:", err)
		}
		if h.workspaceCount() != 1 {
			t.Fatal("a failed native delete removed the workspace row")
		}
	}
	ref := pushgate.Reference(a.Creds, h.cfg.Git.PushOrigin, w.ID)
	secret, err := a.Cluster.CoreV1().Secrets(authtest.Namespace).Get(bg, ref.GetSecretRef().GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	credential := secret.Data[ref.GetSecretRef().GetKey()]
	gate := pushgate.NewService(h.svc.Pool, h.client, u, nil)
	server := &pushgate.Server{Service: gate, Enabled: true, Dev: true, SpoolRoot: filepath.Join(root, "spool"), Limits: pushgate.Limits{BodyBytes: 1 << 20, DiskBytes: 8 << 20}}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	// Git reads the credential from the fake workspace-held Secret. Do not put it in argv/logs.
	cmd := exec.CommandContext(bg, "git", "-C", work, "push", proxy.URL+"/oldsj/mainloop.git", w.Branch)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: " + string(credential), "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if bytes.Contains(out, credential) {
		t.Fatal("git exposed the workspace credential")
	}
	if err == nil {
		if got := git(u.bare, "rev-parse", "refs/heads/"+w.Branch); got != head {
			t.Fatal("gate did not publish the workspace head")
		}
		t.Error("workspace retained publication authority after deletion intent")
	} else {
		check := exec.CommandContext(bg, "git", "--git-dir="+u.bare, "show-ref", "--verify", "refs/heads/"+w.Branch)
		if check.Run() == nil {
			t.Fatal("ref was published despite push refusal")
		}
	}
	if err = h.svc.releaseEndedPush(bg); err != nil {
		t.Fatal(err)
	}
	if reviewProbe(gate, string(credential), "oldsj/mainloop").Code == 200 {
		t.Fatal("credential still usable after release sweep")
	}
	if pushCredentials(h, w.ID) != 1 {
		t.Fatal("push created another credential")
	}
}

// A delete can commit between Enroll reading its authority and planning the grant. If the process
// then dies before the enrollment fence, the grant and credential stay published behind a revoked
// reservation; the release sweep must still settle them and remove the Secret.
func TestReviewLatePlanAfterDeleteIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w, a, u := queuedPushWorkspace(t, h, "feature/test", true)
	createPushSession(t, h)
	activatePushAttempt(h, w.ID)
	gate := pushgate.NewService(h.svc.Pool, h.svc.Kagent, u, a.Creds)
	// An uncommitted grant row holds Enroll's plan insert until the delete has committed.
	tx, err := h.svc.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(bg, `INSERT INTO git_push_grants (workspace_id,branch,policy_version) VALUES ($1,'blocker',1)`, w.ID); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := gate.Enroll(bg, w.ID) // the enrollment fence never runs: the crash
		errc <- err
	}()
	eventually(t, "Enroll blocked on the grant insert", func() bool {
		var n int
		_ = h.svc.Pool.QueryRow(bg, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&n)
		return n > 0
	})
	if _, err = h.svc.enqueue(bg, w.ID, dbstate.OpDelete, nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	if err = <-errc; err != nil {
		t.Fatalf("late Enroll: %v", err)
	}
	name := a.Creds.SecretName(dbstate.CredentialGitPush, w.ID)
	g, err := h.dq.GetPushGrant(bg, w.ID)
	if err != nil || g.State != dbstate.GitGrantPublished || a.Cluster.UID(authtest.Namespace, name) == "" {
		t.Fatal("fixture did not leave a published grant and Secret behind the revoked reservation")
	}
	if r, rerr := h.dq.GetWorkspacePushReservation(bg, w.ID); rerr != nil || r.State != string(dbstate.GitGrantRevoked) {
		t.Fatal("fixture reservation is not revoked")
	}
	runCredentialCleanup(t, h, a)
	for range 2 { // idempotent
		if err = h.svc.releaseEndedPush(bg); err != nil {
			t.Fatal(err)
		}
	}
	if g, err = h.dq.GetPushGrant(bg, w.ID); err != nil || g.State != dbstate.GitGrantRevoked {
		t.Fatal("sweep left the late grant unrevoked")
	}
	for _, c := range h.credentialRows(w.ID) {
		if c.Kind == dbstate.CredentialGitPush && c.State != dbstate.CredentialRevoked && c.State != dbstate.CredentialDeleted {
			t.Fatal("sweep left the late push credential unrevoked")
		}
	}
	eventually(t, "the late push Secret is removed", func() bool { return a.Cluster.UID(authtest.Namespace, name) == "" })
}
