package sessions_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// stallEnv is an env whose stall watch runs on a clock the test moves: StallAfter 10 minutes, and
// an hour while a tool runs.
func stallEnv(t *testing.T) (*env, *atomic.Int64) {
	t.Helper()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) {
		o.StallAfter, o.StallToolAfter, o.ActivityWriteEvery = 10*time.Minute, time.Hour, time.Millisecond
	})
	var skew atomic.Int64
	e.svc.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	return e, &skew
}

type stallRow struct {
	activityAt *time.Time
	kind       *string
	stalledAt  *time.Time
}

func (e *env) stallRow(id string) stallRow {
	e.t.Helper()
	var r stallRow
	if err := e.pool.QueryRow(context.Background(),
		`SELECT activity_at, activity_kind, stalled_at FROM deliveries WHERE id = $1`, id).Scan(&r.activityAt, &r.kind, &r.stalledAt); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) waitStall(id, what string, ok func(stallRow) bool) stallRow {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := e.stallRow(id)
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s: %+v", what, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) markStalled() {
	e.t.Helper()
	if err := e.svc.MarkStalled(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) mainNative() *api.NativeSessionInfo {
	e.t.Helper()
	r, err := e.API().GetMainThread(context.Background(), api.GetMainThreadRequestObject{})
	if err != nil {
		e.t.Fatal(err)
	}
	return api.MainThreadInfo(r.(api.GetMainThread200JSONResponse)).Native
}

func (e *env) stalledAttention() (open int) {
	e.t.Helper()
	svc := &attention.Service{Pool: e.pool, OwnerID: owner, Log: quiet()}
	if err := svc.SyncTasks(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attention_items WHERE source_kind = 'turn_stalled' AND state = 'open'`).Scan(&open); err != nil {
		e.t.Fatal(err)
	}
	return open
}

func working(id string) *a2a.Task {
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

// A turn that goes quiet past its allowance is marked stalled, shown on the thread and raised in the
// owner's attention; the next task event clears all of it. Nothing about the delivery changes.
func TestATurnStallsAfterItsAllowanceAndTheNextEventClearsIt(t *testing.T) {
	e, skew := stallEnv(t)
	task := working("t-quiet")
	e.srv.AddTask(task)
	e.srv.Script(
		kagenttest.Emit{Event: task},
		kagenttest.Emit{Event: agentArtifact("a1", "Thinking it over", false)},
		kagenttest.Pause{D: 1500 * time.Millisecond},
		kagenttest.Emit{Event: agentArtifact("a1", " some more", true)},
		kagenttest.Hang{},
	)
	e.run()
	id := e.send("think hard")
	e.waitState(id, dbstate.DeliveryDelivered)
	first := e.waitStall(id, "activity recorded", func(r stallRow) bool { return r.activityAt != nil })
	if first.kind == nil || *first.kind != string(dbstate.ActivityEvent) {
		t.Fatalf("kind %v", first.kind)
	}

	// Just inside the allowance: not stalled.
	skew.Store(int64(9 * time.Minute))
	e.markStalled()
	if r := e.stallRow(id); r.stalledAt != nil {
		t.Fatalf("stalled inside the allowance: %+v", r)
	}
	// Past it: stalled, on the thread, in attention and in the health view.
	skew.Store(int64(11 * time.Minute))
	e.markStalled()
	if r := e.stallRow(id); r.stalledAt == nil {
		t.Fatal("not stalled past the allowance")
	}
	n := e.mainNative()
	if n.StalledTurn == nil || n.StalledTurn.MessageId != id || !n.StalledTurn.MainThread || n.StalledTurn.ToolRunning {
		t.Fatalf("stalled turn %+v", n.StalledTurn)
	}
	if !n.TurnInFlight || e.delivery(id).State != dbstate.DeliveryDelivered {
		t.Fatal("a stall changed the turn")
	}
	if got := e.stalledAttention(); got != 1 {
		t.Fatalf("%d stalled-turn attention items", got)
	}
	view, err := e.healthService().View(context.Background(), health.Filter{Hours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if view.StalledTurns == nil || len(*view.StalledTurns) != 1 || (*view.StalledTurns)[0].MessageId != id {
		t.Fatalf("health stalled turns %+v", view.StalledTurns)
	}

	// The next event (after the scripted pause) clears it.
	e.waitStall(id, "stall cleared by the next event", func(r stallRow) bool { return r.stalledAt == nil })
	if n := e.mainNative(); n.StalledTurn != nil {
		t.Fatalf("still shows stalled: %+v", n.StalledTurn)
	}
	if got := e.stalledAttention(); got != 0 {
		t.Fatalf("%d stalled-turn attention items after it cleared", got)
	}
	// And nothing was sent again.
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
}

// A tool call that started and has no result yet gets the longer allowance: a long web search or
// build is not a stall until that allowance passes too.
func TestNoFalseStallWhileAToolRunsWithinItsAllowance(t *testing.T) {
	e, skew := stallEnv(t)
	task := working("t-live")
	e.srv.AddTask(task)
	e.srv.Script(
		kagenttest.Emit{Event: task},
		kagenttest.Emit{Event: toolArtifact("function_call", map[string]any{
			"id": "c1", "name": "WebSearch", "args": map[string]any{"query": "x"}})},
		kagenttest.Hang{},
	)
	e.run()
	id := e.send("search")
	e.waitStall(id, "tool running", func(r stallRow) bool {
		return r.kind != nil && *r.kind == string(dbstate.ActivityTool)
	})
	skew.Store(int64(30 * time.Minute))
	e.markStalled()
	if r := e.stallRow(id); r.stalledAt != nil {
		t.Fatalf("a running tool stalled inside its allowance: %+v", r)
	}
	skew.Store(int64(61 * time.Minute))
	e.markStalled()
	if r := e.stallRow(id); r.stalledAt == nil {
		t.Fatal("a tool silent past its allowance is not stalled")
	}
	if n := e.mainNative(); n.StalledTurn == nil || !n.StalledTurn.ToolRunning {
		t.Fatalf("stalled turn %+v", n.StalledTurn)
	}
}

// A task parked for the owner's answer is quiet by design and never stalls.
func TestAParkedTurnNeverStalls(t *testing.T) {
	e, skew := stallEnv(t)
	parked := &a2a.Task{ID: "t-ask", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	e.srv.AddTask(parked)
	e.srv.Script(
		kagenttest.Emit{Event: working("t-ask")},
		kagenttest.Emit{Event: a2a.NewStatusUpdateEvent(parked, a2a.TaskStateInputRequired, nil)},
	)
	e.run()
	id := e.send("ask me")
	e.waitStall(id, "parked", func(r stallRow) bool {
		return r.kind != nil && *r.kind == string(dbstate.ActivityParked)
	})
	skew.Store(int64(48 * time.Hour))
	e.markStalled()
	if r := e.stallRow(id); r.stalledAt != nil {
		t.Fatalf("a parked turn stalled: %+v", r)
	}
	if d := e.delivery(id); d.State != dbstate.DeliveryDelivered {
		t.Fatalf("state %s", d.State)
	}
}
