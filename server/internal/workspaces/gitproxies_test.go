package workspaces

import (
	"errors"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	ka "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// proxyReferences counts the read and push Git proxy references on a Session create.
func proxyReferences(creds []*ka.SessionCredential) (read, push int) {
	for _, c := range creds {
		switch {
		case strings.HasPrefix(c.GetOrigin(), "http://mainloop-git-read."):
			read++
		case strings.HasPrefix(c.GetOrigin(), "http://mainloop-git-push."):
			push++
		}
	}
	return read, push
}

// An owner workspace has no task attempt, so nothing may ever write through it, yet kagent prepares
// it only with a push reference: it gets one that no reservation, grant or Secret backs, and it
// reaches a confirmed preparation like any other.
func TestAnEnvironmentWorkspaceWithoutAWritableAttemptIsPreparedWithAnInertPushReference(t *testing.T) {
	h, _, w := gatedWorkspace(t)
	var read, push int
	for _, req := range h.fake.CreateRequests() {
		if req.GetRequestId() == h.row(w.WorkspaceId).RequestID {
			read, push = proxyReferences(req.GetCredentials())
		}
	}
	if read != 1 || push != 1 {
		t.Fatalf("git proxy references = %d read, %d push, want 1 each", read, push)
	}
	var reservations int
	if err := h.svc.Pool.QueryRow(bg, `SELECT count(*) FROM workspace_push_reservations WHERE workspace_id=$1`, w.WorkspaceId).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 || pushCredentials(h, w.WorkspaceId) != 0 {
		t.Fatalf("the unreserved reference has authority: %d reservations, %d push credentials", reservations, pushCredentials(h, w.WorkspaceId))
	}
	h.runPreparation()
	eventually(t, "the preparation to be confirmed", func() bool { return h.prepState(w.WorkspaceId) == dbstate.PreparationConfirmed })
}

// A development environment needs both Git transports; a server without them refuses the create
// instead of making a workspace kagent can never prepare.
func TestAnEnvironmentWorkspaceIsRefusedWhenTheGitTransportsAreNotConfigured(t *testing.T) {
	for _, kind := range []string{"neither", "read only", "gate without upstream"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, nil)
			h.svc.Refs = &fakeRefs{sha: prepSHA}
			if _, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("first")}); err != nil {
				t.Fatal(err)
			}
			if kind != "neither" {
				h.withGitRead()
			}
			if kind == "gate without upstream" {
				// The policy-only gate of a process whose push gate is off cannot enroll.
				h.svc.GitPush = &GitPush{Gate: pushgate.NewService(h.svc.Pool, h.svc.Kagent, nil, h.svc.Credentials), Origin: h.cfg.Git.PushOrigin}
			}
			selectEnvironment(h)
			n, calls := h.workspaceCount(), len(h.fake.CreateRequests())
			_, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("second")})
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(inv.Error(), "git_transport_required") {
				t.Fatalf("err = %v", err)
			}
			if h.workspaceCount() != n || len(h.fake.CreateRequests()) != calls {
				t.Fatalf("a refused create left state: workspaces %d -> %d, kagent creates %d -> %d", n, h.workspaceCount(), calls, len(h.fake.CreateRequests()))
			}
		})
	}
}

// A row that got its environment while this server could not carry the references (config lost
// between planning and the create) is rejected with the reason, not retried for half an hour.
func TestAnEnvironmentWorkspaceWhoseGitTransportsWereLostIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	w, err := h.create(api.CreateWorkspaceRequest{Branch: ptr("feature")})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspaces SET environment=$2 WHERE id=$1`, w.WorkspaceId, testEnvironment)
	createPushSession(t, h)
	if op := h.op(w.WorkspaceId, "create"); op.State != "rejected" || op.LastError == nil || !strings.Contains(*op.LastError, "git_transport_required") {
		t.Fatalf("create op = %s, %v", op.State, op.LastError)
	}
	if len(h.fake.CreateRequests()) != 0 {
		t.Fatal("a Session was made that kagent can never prepare")
	}
}
