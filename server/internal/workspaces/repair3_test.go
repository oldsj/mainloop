package workspaces

import (
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

// A workspace whose create landed (with its credential reference) but whose reply was lost, then
// the owner cancels the session, which revokes the credential, and deletes the workspace. The delete
// replays the create to find the Session; kagent refuses a replay whose credentials differ, so the
// replay must carry the same reference even though the credential is revoked.
func TestADeleteReplayAfterCancelCarriesTheCreatesCredential(t *testing.T) {
	h := newHarness(t, slow)
	h.withCredentials()
	ss := h.ledger()
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The create doCreate sent never reached the fake's log, so send the same one.
	req := h.row(w.WorkspaceId).createRequest()
	creds, err := h.svc.Credentials.SessionCredentials(bg, w.WorkspaceId, h.cfg.MCPOrigin())
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("create credentials = %v", creds)
	}
	req.Credentials = creds
	landed, err := h.client.CreateSession(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Cancel(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.credentialRows(w.WorkspaceId) {
		if c.State == dbstate.CredentialPublished {
			t.Fatalf("credential still published after cancel: %+v", c)
		}
	}
	h.svc.Config.Workspaces.OperationWait = 5e9
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reqs := h.fake.CreateRequests()
	last := reqs[len(reqs)-1]
	if len(last.GetCredentials()) != 1 || last.GetCredentials()[0].GetSecretRef().GetName() != creds[0].GetSecretRef().GetName() {
		t.Fatalf("delete replay credentials = %v, create's = %v", last.GetCredentials(), creds)
	}
	if _, alive := h.fake.Session(landed.GetId()); alive {
		t.Error("the Session the lost-reply create made is still alive")
	}
	if h.workspaceCount() != 0 {
		t.Errorf("%d workspace rows left", h.workspaceCount())
	}
}

// A workspace whose create landed but whose reply was lost, then gave up; the owner cancels and
// archives its session. The sweep starts a delete op for it (it has no Session id to delete by), the
// op replays the create and deletes the Session, and the binding stops blocking the orphan sweep.
func TestAnArchivedWorkspaceWithAnUnresolvedCreateIsDeleted(t *testing.T) {
	h := newHarness(t, map[string]string{
		"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS": "30",
		"WORKSPACE_CREATE_RETRY_MAX_SECONDS":     "30",
		"WORKSPACE_OPERATION_WAIT_SECONDS":       "0.2",
		"WORKSPACE_ORPHAN_MIN_AGE_SECONDS":       "1",
	})
	ss := h.ledger()
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	landed, err := h.client.CreateSession(bg, h.row(w.WorkspaceId).createRequest())
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET first_failed_at = now() - interval '601 seconds'`)
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	h.due()
	eventually(t, "gave up", func() bool { return h.op(w.WorkspaceId, "create").State == dbstate.OpGaveUp })

	if _, err := ss.Cancel(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if err := ss.Archive(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if n, err := h.dq.CountUnresolvedBindings(bg); err != nil || n != 1 {
		t.Fatalf("unresolved before the sweep = %d, %v; want the one create that may have landed", n, err)
	}
	// Recovering the Session id stops the binding counting as unresolved before the asynchronous
	// delete finishes. Wait for the Session itself to be gone as well.
	eventually(t, "the sweep deletes the Session and settles the binding", func() bool {
		_ = h.svc.DeleteArchived(bg)
		_ = ss.ResolveCreates(bg)
		n, err := h.dq.CountUnresolvedBindings(bg)
		_, alive := h.fake.Session(landed.GetId())
		return err == nil && n == 0 && !alive && h.workspaceCount() == 0
	})
	if _, alive := h.fake.Session(landed.GetId()); alive {
		t.Error("the Session the lost-reply create made is still alive")
	}
	time.Sleep(1100 * time.Millisecond)
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || rep.Blocked != "" {
		t.Fatalf("orphan sweep: blocked=%q err=%v", rep.Blocked, err)
	}
}

// An archived workspace whose create is still pending is left alone: its create op decides.
func TestAnArchivedWorkspaceWithAnOpenCreateIsNotDeletedBySweep(t *testing.T) {
	h := newHarness(t, slow)
	ss := h.ledger()
	w, err := h.create(api.CreateWorkspaceRequest{}) // no runner: the create op stays open
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET attempts = 1`)
	if _, err := ss.Cancel(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if err := ss.Archive(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeleteArchived(bg); err != nil {
		t.Fatal(err)
	}
	if op := h.op(w.WorkspaceId, "create"); op.State != dbstate.OpPending && op.State != dbstate.OpRetrying {
		t.Fatalf("create op is %s", op.State)
	}
	if h.workspaceCount() != 1 {
		t.Fatal("the sweep deleted a workspace whose create is still open")
	}
}
