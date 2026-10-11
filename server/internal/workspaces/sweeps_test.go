package workspaces

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/kagent"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func (h *harness) backdate(id string) {
	h.t.Helper()
	h.exec(`UPDATE workspaces SET last_active_at = now() - interval '2 hours', created_at = now() - interval '3 hours' WHERE id = $1`, id)
}

func (h *harness) suspendOps() int {
	h.t.Helper()
	var n int
	if err := h.svc.Pool.QueryRow(bg, `SELECT count(*) FROM workspace_ops WHERE kind = 'suspend'`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// Mined: test_a_quiet_ready_session_is_suspended, test_an_already_suspended_session_is_not_suspended_again.
func TestIdleWorkspaceIsSuspendedOnce(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 0 {
		t.Fatalf("a fresh workspace was picked: %v", err)
	}
	h.backdate(w.WorkspaceId)
	h.run()
	if err := h.svc.SuspendIdle(bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, "suspended", func() bool {
		got, _ := h.svc.Get(bg, w.WorkspaceId)
		return got.ObservedState == api.WorkspaceObservedStateSuspended
	})
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 1 {
		t.Fatalf("suspended again: ops %d, %v", h.suspendOps(), err)
	}
}

// Mined: test_activity_after_the_idle_check_picked_it_is_not_suspended.
func TestActivityAfterTheIdleCheckKeepsItRunning(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.backdate(w.WorkspaceId)
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 1 {
		t.Fatalf("%v, ops %d", err, h.suspendOps())
	}
	if err := h.svc.Touch(bg, w.WorkspaceId); err != nil { // traffic arrives before the op runs
		t.Fatal(err)
	}
	h.run()
	eventually(t, "cancelled", func() bool { return h.op(w.WorkspaceId, "suspend").State == "cancelled" })
	if h.fake.Count(kagent.ProcSuspendSession) != 0 {
		t.Fatal("suspend reached kagent")
	}
}

// Mined: test_an_active_turn_is_left_alone, test_a_message_arriving_during_suspend_waits_then_resumes_before_sending.
func TestIdleCheckSkipsBusyWorkspaces(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.backdate(w.WorkspaceId)
	h.dl.setOpen(w.WorkspaceId, true)
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 0 {
		t.Fatalf("an open turn was suspended: %v", err)
	}
	h.dl.setOpen(w.WorkspaceId, false)
	h.dl.mu.Lock()
	h.dl.active[w.WorkspaceId] = time.Now() // a delivery finished just now
	h.dl.mu.Unlock()
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 0 {
		t.Fatalf("recent delivery activity was ignored: %v", err)
	}
}

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

// Mined (wake on preview): test_a_suspended_workspace_is_resumed_and_published,
// test_a_running_workspace_is_left_alone, test_concurrent_previews_resume_once.
func TestWakeForPreviewResumesOnce(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.run()
	if err := h.svc.WakeForPreview(bg, w.WorkspaceId); err != nil || h.fake.Count(kagent.ProcResumeSession) != 0 {
		t.Fatalf("a running workspace was resumed: %v", err)
	}
	if _, err := h.svc.Suspend(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	events := h.events()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- h.svc.WakeForPreview(bg, w.WorkspaceId) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("resume calls: %d", n)
	}
	got, _ := h.svc.Get(bg, w.WorkspaceId)
	if got.ObservedState != api.WorkspaceObservedStateRunning || h.events() != events+1 {
		t.Fatalf("%+v events %d -> %d", got, events, h.events())
	}
}

// Mined: test_a_failed_resume_raises_and_the_next_preview_retries.
func TestFailedWakeIsRetried(t *testing.T) {
	h := newHarness(t, slow)
	w := h.running()
	h.run()
	if _, err := h.svc.Suspend(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	h.fake.FailNext(kagent.ProcResumeSession, kagenttestDrop)
	var unc *UnconfirmedError
	if err := h.svc.WakeForPreview(bg, w.WorkspaceId); !errors.As(err, &unc) {
		t.Fatalf("%v", err)
	}
	h.due()
	eventually(t, "running", func() bool {
		got, _ := h.svc.Get(bg, w.WorkspaceId)
		return got.ObservedState == api.WorkspaceObservedStateRunning
	})
	if err := h.svc.WakeForPreview(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
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
