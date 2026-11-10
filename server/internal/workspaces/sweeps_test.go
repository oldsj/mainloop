package workspaces

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// Mined: test_archive_deletes_the_kagent_session, test_archive_waits_for_an_open_turn_and_the_reconcile_pass_deletes_after,
// test_an_unconfirmed_archive_delete_is_retried_by_the_reconcile_pass.
func TestArchivedSessionsAreDeletedInKagent(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	if err := h.svc.DeleteArchived(bg); err != nil || len(h.sessions()) != 1 {
		t.Fatalf("a live session was deleted: %v", err)
	}
	if _, err := h.dq.ArchiveSession(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	h.dl.setOpen(w.WorkspaceId, true)
	if err := h.svc.DeleteArchived(bg); err != nil || len(h.sessions()) != 1 {
		t.Fatalf("deleted under an open turn: %v", err)
	}
	h.dl.setOpen(w.WorkspaceId, false)
	h.deleteState.Store(int32(kagentapi.RuntimeState_RUNTIME_STATE_READY)) // kagent does not confirm
	if err := h.svc.DeleteArchived(bg); err == nil {
		t.Fatal("an unconfirmed delete passed")
	}
	if deleted(h, w.WorkspaceId) {
		t.Fatal("marked deleted without confirmation")
	}
	if err := h.svc.DeleteArchived(bg); err != nil { // the Session is gone now: NotFound confirms
		t.Fatal(err)
	}
	if !deleted(h, w.WorkspaceId) {
		t.Fatal("not marked deleted")
	}
	if err := h.svc.DeleteArchived(bg); err != nil || h.fake.Count(kagent.ProcDeleteSession) != 2 {
		t.Fatalf("%v, delete calls %d", err, h.fake.Count(kagent.ProcDeleteSession))
	}
}

func deleted(h *harness, id string) bool {
	h.t.Helper()
	var ok bool
	if err := h.svc.Pool.QueryRow(bg, `SELECT kagent_deleted_at IS NOT NULL FROM agent_bindings WHERE session_id = $1`, id).Scan(&ok); err != nil {
		h.t.Fatal(err)
	}
	return ok
}

func (h *harness) strayOn(namespace, name, requestID string) string {
	h.t.Helper()
	s, err := h.client.CreateSession(bg, &kagentapi.CreateSessionRequest{
		Agent: &kagentapi.ResourceReference{Namespace: namespace, Name: name}, RequestId: requestID,
		Workspace: &kagentapi.Workspace{Repo: "https://github.com/oldsj/stray", Ref: "main", Branch: "x"},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return s.GetId()
}

// Mined (test_orphan_sessions.py): test_it_lists_the_sessions_no_binding_names_and_deletes_nothing,
// test_delete_removes_only_the_orphans, test_sessions_on_other_instances_agents_are_ignored.
func TestOrphanSweepIsScopedAndDryRunByDefault(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.fake.AddAgent("other", "agent")
	orphan := h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	foreign := h.strayOn("other", "agent", "stray-2")
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }

	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{})
	if err != nil || len(rep.Orphans) != 1 || rep.Orphans[0] != orphan || len(h.sessions()) != 3 {
		t.Fatalf("%v %+v", err, rep)
	}
	rep, err = h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || len(rep.Deleted) != 1 || rep.Deleted[0] != orphan {
		t.Fatalf("%v %+v", err, rep)
	}
	left := map[string]bool{}
	for _, s := range h.sessions() {
		left[s.GetId()] = true
	}
	if len(left) != 2 || !left[foreign] || !left[*h.row(w.WorkspaceId).KagentSessionID] {
		t.Fatalf("remaining %v", left)
	}
}

// Mined: test_it_leaves_young_sessions (the minimum age), test_a_session_bound_since_the_listing_is_not_deleted.
func TestOrphanSweepLeavesYoungSessions(t *testing.T) {
	h := newHarness(t, nil)
	h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || len(rep.Orphans) != 0 || len(h.sessions()) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
}

// Mined: test_unresolved_create_blocks_deletion.
func TestUnresolvedCreateBlocksOrphanDeletion(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	if _, err := h.create(api.CreateWorkspaceRequest{}); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET attempts = 1`) // a create was sent and its Session id never recorded
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || rep.Blocked == "" || len(rep.Deleted) != 0 || len(h.sessions()) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
}

// Mined: test_a_failed_delete_is_reported_and_the_exit_status_is_nonzero.
func TestOrphanDeleteFailureIsReported(t *testing.T) {
	h := newHarness(t, nil)
	orphan := h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	h.fake.FailNext(kagent.ProcDeleteSession, kagenttestDrop)
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err == nil || rep.Failed[orphan] == "" {
		t.Fatalf("%v %+v", err, rep)
	}
	var out, errb bytes.Buffer
	if code := printOrphans(rep, err, true, &out, &errb); code != 1 || !bytes.Contains(out.Bytes(), []byte("failed  "+orphan)) {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	var dry bytes.Buffer
	if code := printOrphans(OrphanReport{Orphans: []string{orphan}}, nil, false, &dry, &errb); code != 0 ||
		!bytes.Contains(dry.Bytes(), []byte("orphan  "+orphan)) || !bytes.Contains(dry.Bytes(), []byte("nothing deleted")) {
		t.Fatalf("code %d out %q", code, dry.String())
	}
}

// `sweep-orphans --delete` takes the scheduled orphan sweep's lease: it refuses while that sweep
// holds it, and deletes once it is free.
func TestOrphanDeleteCommandSharesTheSweepLease(t *testing.T) {
	h := newHarness(t, nil)
	orphan := h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }

	h.exec(`INSERT INTO sweep_leases (name, holder, until) VALUES ($1, 'scheduled-sweep/1', now() + interval '1 minute')`, OrphanSweepName)
	rep, ran, err := h.svc.deleteOrphansLeased(bg)
	if err != nil || ran || len(rep.Deleted) != 0 || len(h.sessions()) != 1 {
		t.Fatalf("ran=%v err=%v %+v: deleted while the scheduled sweep held the lease", ran, err, rep)
	}

	h.exec(`DELETE FROM sweep_leases WHERE name = $1`, OrphanSweepName)
	rep, ran, err = h.svc.deleteOrphansLeased(bg)
	if err != nil || !ran || len(rep.Deleted) != 1 || rep.Deleted[0] != orphan {
		t.Fatalf("ran=%v err=%v %+v", ran, err, rep)
	}
}

func TestOrphanSweepNamesAPendingMainCreateAsTheBlock(t *testing.T) {
	h := newHarness(t, nil)
	h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	h.exec(`INSERT INTO sessions (id, user_id, title, description, prompt, status) VALUES ('main-1', 'owner', 'Main', 'd', 'p', 'idle')`)
	h.exec(`INSERT INTO agent_bindings (session_id, kind, role, main_pending, kagent_create_request_id) VALUES ('main-1', 'codex', 'main', true, 'req-1')`)
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || len(rep.Deleted) != 0 || len(h.sessions()) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	if !strings.Contains(rep.Blocked, "1 is a pending main-provider switch") {
		t.Fatalf("blocked reason %q does not name the pending main create", rep.Blocked)
	}
}
