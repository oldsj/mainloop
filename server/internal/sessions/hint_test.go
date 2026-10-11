package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// A turn that parks for the owner shows up as a pending request within a sweep, though the
// observer's scan period is an hour.
func TestAParkedTurnShowsItsRequestWithoutWaitingForTheScanPeriod(t *testing.T) {
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ParkedMax = 100 * time.Millisecond })
	hs := &hitl.Service{
		Pool: e.pool, Kagent: hitl.FromClient(e.other()), OwnerID: owner, Agent: agent, Log: quiet(),
		Opts: hitl.Options{ObserveEvery: 20 * time.Millisecond, ScanEvery: time.Hour, RefreshEvery: time.Hour},
	}
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.DeliveryLoop()
	l.Interval = 20 * time.Millisecond
	r.Add(l)
	r.AddSweep(hs.ObserverSweep())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The first turn creates the session; the observer scans it once and then rests for an hour.
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var scanned bool
		err := e.pool.QueryRow(context.Background(),
			`SELECT scanned_at IS NOT NULL FROM hitl_sessions WHERE kagent_session_id = $1`, ksid).Scan(&scanned)
		if err == nil && scanned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the observer never scanned the session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ask := &a2a.Message{
		ID: "ask-1", Role: a2a.MessageRoleAgent, Extensions: []string{kagent.HITLExtension},
		Metadata: map[string]any{kagent.HITLExtension: map[string]any{
			"type": "tool_approval_request", "hint": "needs a human",
			"tools": []any{map[string]any{"id": "t1", "call_id": "call-t1", "name": "run_t1", "args": map[string]any{"cmd": "ls"}}},
		}},
	}
	parked := &a2a.Task{ID: "t-park", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: ask}}
	e.srv.Script(kagenttest.Emit{Event: parked})
	e.srv.AddTask(parked)
	e.send("two")

	deadline = time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM hitl_requests WHERE task_id = 't-park' AND availability = 'pending'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no pending request for the parked turn within 10 s, though ScanEvery is an hour")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
