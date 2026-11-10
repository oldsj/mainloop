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

// kagent parks an idle workspace's Actor itself and the Session stays READY. A message for it is
// sent without any resume: the send is what wakes the Actor.
func TestAMessageForASleepingWorkspaceWakesItAndIsSentOnce(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.ledger()
	w := h.running()
	h.runBoth(ss)
	ksid := h.row(w.WorkspaceId).sessionID()
	h.fake.Sleep(ksid)
	if h.runtimeSessionState(w.WorkspaceId) != string(api.WorkspaceObservedStateSuspended) {
		t.Fatal("a sleeping workspace does not show as suspended")
	}

	id := uuid.NewString()
	if _, err := ss.SendTo(bg, w.WorkspaceId, id, "continue please"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivered and finished", func() bool { return h.deliveryState(id) == dbstate.DeliveryCompleted })
	if n := h.fake.Count("SendStreamingMessage"); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	if h.fake.Wakes(ksid) != 1 || h.runtimeSessionState(w.WorkspaceId) != string(api.WorkspaceObservedStateRunning) {
		t.Fatalf("the send did not wake the workspace: %d wakes", h.fake.Wakes(ksid))
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
