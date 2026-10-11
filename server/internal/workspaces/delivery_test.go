package workspaces

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// ledger wires the real delivery ledger into the harness: the workspace service is its Waker, and
// the ledger is the workspace service's Deliveries.
func (h *harness) ledger() *sessions.Service {
	h.t.Helper()
	ref := kagent.AgentRef{Namespace: "kagent", Name: "mainloop-main"}
	h.fake.AddAgent(ref.Namespace, ref.Name)
	ss := &sessions.Service{
		Pool: h.svc.Pool, Kagent: h.client, OwnerID: owner, Agent: ref, Log: h.svc.Log,
		Providers: h.svc.Providers, Waker: h.svc,
		Opts: sessions.Options{
			UncertainHold: time.Minute, UncertainRecheck: 20 * time.Millisecond,
			FollowPoll: 20 * time.Millisecond, Lease: 2 * time.Second, Concurrency: 4,
		},
	}
	h.svc.Deliveries = ss
	return ss
}

// runBoth runs the workspace loop and the delivery loop under one runner.
func (h *harness) runBoth(ss *sessions.Service) {
	h.t.Helper()
	r := reconcile.NewRunner(h.svc.Pool, h.svc.Log, nil)
	wl, dl := h.svc.Loop(), ss.DeliveryLoop()
	wl.Interval, dl.Interval = 10*time.Millisecond, 20*time.Millisecond
	r.Add(wl)
	r.Add(dl)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
}

func (h *harness) deliveryState(id string) dbstate.DeliveryState {
	h.t.Helper()
	d, err := dbq.New(h.svc.Pool).GetDelivery(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d.State
}

func (h *harness) runtimeSessionState(id string) string {
	h.t.Helper()
	got, err := h.svc.Get(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(got.ObservedState)
}

// Mined: test_suspended_session_is_resumed_before_the_turn.
func TestAMessageForASuspendedWorkspaceResumesItFirstAndIsSentOnce(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	h.runBoth(ss)
	if _, err := h.svc.Suspend(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if h.runtimeSessionState(w.WorkspaceId) != string(api.WorkspaceObservedStateSuspended) {
		t.Fatal("not suspended")
	}

	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "continue please"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivered and finished", func() bool { return h.deliveryState(id) == dbstate.DeliveryCompleted })
	if n := h.fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	if n := h.fake.Count("SendStreamingMessage"); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	if h.runtimeSessionState(w.WorkspaceId) != string(api.WorkspaceObservedStateRunning) {
		t.Fatal("the send went to a Session that was not running")
	}
}

// Mined: test_a_message_arriving_during_suspend_waits_then_resumes_before_sending.
func TestAMessageDuringAnOpenSuspendWaitsForItThenResumes(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	h.fake.FailNext(kagent.ProcSuspendSession, kagenttest.Fault{Delay: 600 * time.Millisecond}) // the suspend is in flight
	h.runBoth(ss)
	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpSuspend, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "suspend reached kagent", func() bool { return h.fake.Count(kagent.ProcSuspendSession) == 1 })

	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "hello"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := h.fake.Count("SendStreamingMessage"); n != 0 || h.fake.Count(kagent.ProcResumeSession) != 0 {
		t.Fatalf("the send did not wait: %d sends, %d resumes", n, h.fake.Count(kagent.ProcResumeSession))
	}
	eventually(t, "finished", func() bool { return h.deliveryState(id) == dbstate.DeliveryCompleted })
	if h.fake.Count(kagent.ProcSuspendSession) != 1 || h.fake.Count(kagent.ProcResumeSession) != 1 {
		t.Fatalf("%d suspends, %d resumes", h.fake.Count(kagent.ProcSuspendSession), h.fake.Count(kagent.ProcResumeSession))
	}
	if n := h.fake.Count("SendStreamingMessage"); n != 1 {
		t.Fatalf("sent %d times", n)
	}
}

// Mined: test_an_active_turn_is_left_alone.
func TestIdleSuspendLeavesASessionWithAnOpenTurn(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	h.backdate(w.WorkspaceId)
	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "working"); err != nil { // recorded; no loop runs it
		t.Fatal(err)
	}
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 0 {
		t.Fatalf("an open turn was suspended: %v", err)
	}
	// Finished just now: still not idle.
	h.exec(`UPDATE deliveries SET state = 'completed', updated_at = now() WHERE id = $1`, id)
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 0 {
		t.Fatalf("a turn that just finished counted as idle: %v", err)
	}
	h.exec(`UPDATE deliveries SET updated_at = now() - interval '2 hours' WHERE id = $1`, id)
	if err := h.svc.SuspendIdle(bg); err != nil || h.suspendOps() != 1 {
		t.Fatalf("a quiet session was kept: ops %d, %v", h.suspendOps(), err)
	}
}

func TestHasOpenAndLastActivity(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	if open, err := ss.HasOpen(bg, w.WorkspaceId); err != nil || open {
		t.Fatalf("%v %v", open, err)
	}
	if at, err := ss.LastActivity(bg, w.WorkspaceId); err != nil || !at.IsZero() {
		t.Fatalf("%v %v", at, err)
	}
	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "x"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []dbstate.DeliveryState{dbstate.DeliveryRecorded, dbstate.DeliverySending, dbstate.DeliveryDelivered} {
		h.exec(`UPDATE deliveries SET state = $2 WHERE id = $1`, id, string(state))
		if open, err := ss.HasOpen(bg, w.WorkspaceId); err != nil || !open {
			t.Fatalf("%s: %v %v", state, open, err)
		}
	}
	if at, err := ss.LastActivity(bg, w.WorkspaceId); err != nil || time.Since(at) > time.Minute {
		t.Fatalf("%v %v", at, err)
	}
	h.exec(`UPDATE deliveries SET state = 'completed' WHERE id = $1`, id)
	if open, _ := ss.HasOpen(bg, w.WorkspaceId); open {
		t.Fatal("a completed turn is open")
	}
	h.exec(`UPDATE deliveries SET state = 'uncertain', updated_at = now() WHERE id = $1`, id)
	if open, _ := ss.HasOpen(bg, w.WorkspaceId); !open {
		t.Fatal("an uncertain turn inside its hold is closed")
	}
	h.exec(`UPDATE deliveries SET updated_at = now() - interval '2 hours' WHERE id = $1`, id)
	if open, _ := ss.HasOpen(bg, w.WorkspaceId); open {
		t.Fatal("an uncertain turn past its hold is open")
	}
}

// skipFirstWake is a Waker whose first call does nothing, as when something suspends the workspace
// between the wake and the check that follows it.
type skipFirstWake struct {
	inner sessions.Waker
	calls int
}

func (w *skipFirstWake) Wake(ctx context.Context, id string) error {
	w.calls++
	if w.calls == 1 {
		return nil
	}
	return w.inner.Wake(ctx, id)
}

// A workspace's Session is never resumed outside its operation ledger, even when the delivery finds
// it suspended right after a wake: it wakes the workspace again.
func TestADeliveryThatFindsTheWorkspaceSuspendedAfterWakeWakesAgain(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	h.runBoth(ss)
	if _, err := h.svc.Suspend(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	waker := &skipFirstWake{inner: h.svc}
	ss.Waker = waker

	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "continue please"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivered and finished", func() bool { return h.deliveryState(id) == dbstate.DeliveryCompleted })
	if waker.calls < 2 {
		t.Fatalf("%d wakes: the delivery resumed the Session itself", waker.calls)
	}
	if n := h.fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	var resumes int
	if err := h.svc.Pool.QueryRow(bg, `SELECT count(*) FROM workspace_ops WHERE workspace_id = $1 AND kind = 'resume'`, w.WorkspaceId).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 1 {
		t.Fatalf("%d resume ops: the resume did not go through the ledger", resumes)
	}
}

// A delivery waiting for a workspace that never gets its Session gives up after the hold instead of
// holding the turn open for ever.
func TestADeliveryForAWorkspaceWithoutASessionGivesUp(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "hello"); err != nil { // recorded; no loop runs it yet
		t.Fatal(err)
	}
	h.exec(`UPDATE agent_bindings SET kagent_session_id = NULL WHERE session_id = $1`, w.WorkspaceId)
	h.runBoth(ss)
	time.Sleep(200 * time.Millisecond)
	if st := h.deliveryState(id); st != dbstate.DeliveryRecorded {
		t.Fatalf("within the hold the delivery was %s, want it waiting", st)
	}
	h.exec(`UPDATE deliveries SET created_at = now() - interval '2 hours' WHERE id = $1`, id)
	eventually(t, "given up", func() bool { return h.deliveryState(id) == dbstate.DeliveryFailed })
	if n := h.fake.Count("SendStreamingMessage"); n != 0 {
		t.Fatalf("sent %d times", n)
	}
}
