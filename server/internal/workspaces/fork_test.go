package workspaces

import (
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// A forked session (checkpoints.go's commitFork, issue #311) inserts a workspace row already
// bound to the kagent Session ForkSession made, with no create operation ever sent: the binding
// is bound at the same time the row is written. This builds that shape directly with SQL, the
// same way the other packages' tests stand in for a collaborator they do not import, since
// checkpoints.go lives in the sessions package and this package's tests do not wire that one in.
func (h *harness) insertBoundWorkspace(id, kagentSessionID string) {
	h.t.Helper()
	h.exec(`INSERT INTO sessions (id,user_id,title,description,prompt) VALUES ($1,$2,'t','d','p')`, id, owner)
	h.exec(`INSERT INTO agent_bindings (session_id,kind,role,mcp_grant_kind,kagent_session_id) VALUES ($1,'claude','agent','none',$2)`,
		id, kagentSessionID)
	h.exec(`INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ($1,$2,'o','n','o/n','https://github.com/o/n')`,
		"p-"+id, owner)
	h.exec(`INSERT INTO workspaces (id,user_id,project_id,repo_url,branch,agent_kind,agent_namespace,agent_name,request_id)
	    VALUES ($1,$2,$3,'https://github.com/o/n','b','claude','kagent','agent',$4)`, id, owner, "p-"+id, newID())
}

// Deleting a fork's workspace deletes its kagent Session through the ordinary workspace delete
// path: the reconciler rule's intent row (workspace_ops) is the same one any delete writes, and
// doDelete finds the Session already bound (sid != "") and deletes it directly, with no create
// replay (there was never a create to replay).
func TestDeleteReleasesAForkedWorkspacesKagentSession(t *testing.T) {
	h := newHarness(t, nil)
	sess, err := h.client.CreateSession(bg, &kagentapi.CreateSessionRequest{RequestId: newID()})
	if err != nil {
		t.Fatal(err)
	}
	id := newID()
	h.insertBoundWorkspace(id, sess.GetId())

	stop := h.run()
	defer stop()
	if err := h.svc.Delete(bg, id); err != nil {
		t.Fatal(err)
	}
	if n := h.workspaceCount(); n != 0 {
		t.Fatalf("workspace rows remain: %d", n)
	}
	if n := h.fake.Count(kagent.ProcDeleteSession); n != 1 {
		t.Fatalf("delete did not reach kagent: %d calls", n)
	}
	if len(h.sessions()) != 0 {
		t.Fatal("kagent still has the session")
	}
	// No create was ever sent for this workspace, replayed or otherwise: the only CreateSession
	// call in this test is the direct one that stood in for ForkSession above.
	if n := h.fake.Count(kagent.ProcCreateSession); n != 1 {
		t.Fatalf("delete should not have sent a create of its own: %d calls", n)
	}
}
