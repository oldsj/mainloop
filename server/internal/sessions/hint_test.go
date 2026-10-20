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

// parkEnv is a sessions environment with the delivery loop and the HITL observer (and, with
// answers, the dispatcher) running. The observer's scan period is an hour, so only a hint or the
// first scan of a new session makes it list tasks.
type parkEnv struct {
	*env
	hs *hitl.Service
}

func newParkEnv(t *testing.T, parkedMax time.Duration, answers bool, kg func(hitl.Kagent, *kagenttest.Server) hitl.Kagent) *parkEnv {
	t.Helper()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ParkedMax = parkedMax })
	k := hitl.FromClient(e.other())
	if kg != nil {
		k = kg(k, e.srv)
	}
	hs := &hitl.Service{
		Pool: e.pool, Kagent: k, OwnerID: owner, Agent: agent, Log: quiet(), WritesEnabled: true, Turns: e.svc,
		Opts: hitl.Options{ObserveEvery: 20 * time.Millisecond, ScanEvery: time.Hour, RefreshEvery: time.Hour},
	}
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.DeliveryLoop()
	l.Interval = 20 * time.Millisecond
	r.Add(l)
	r.AddSweep(hs.ObserverSweep())
	if answers {
		rl := hs.ResponseLoop()
		rl.Interval = 20 * time.Millisecond
		r.Add(rl)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &parkEnv{env: e, hs: hs}
}

// firstTurn completes one turn, which creates the session, and waits until the observer has scanned
// it once and rests for an hour. It returns the kagent session id.
func (p *parkEnv) firstTurn() string {
	p.t.Helper()
	p.waitState(p.send("one"), dbstate.DeliveryCompleted)
	ksid := p.kagentSessionID()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var scanned bool
		err := p.pool.QueryRow(context.Background(),
			`SELECT scanned_at IS NOT NULL FROM hitl_sessions WHERE kagent_session_id = $1`, ksid).Scan(&scanned)
		if err == nil && scanned {
			return ksid
		}
		if time.Now().After(deadline) {
			p.t.Fatal("the observer never scanned the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *parkEnv) count(query string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *parkEnv) waitCount(want int, what, query string, args ...any) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.count(query, args...) != want {
		if time.Now().After(deadline) {
			p.t.Fatalf("%s: want %d within 10 s", what, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// parkedTask is a task parked for the owner. kind is the A2A state; the ask is a tool approval.
func parkedTask(id, contextID, askID string, state a2a.TaskState) *a2a.Task {
	ask := &a2a.Message{
		ID: askID, Role: a2a.MessageRoleAgent, Extensions: []string{kagent.HITLExtension},
		Metadata: map[string]any{kagent.HITLExtension: map[string]any{
			"type": "tool_approval_request", "hint": "needs a human",
			"tools": []any{map[string]any{"id": "t1", "call_id": "call-t1", "name": "run_t1", "args": map[string]any{"cmd": "ls"}}},
		}},
	}
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: contextID, Status: a2a.TaskStatus{State: state, Message: ask}}
}

// A turn that parks for the owner shows up as a pending request within a sweep, though the
// observer's scan period is an hour.
func TestAParkedTurnShowsItsRequestWithoutWaitingForTheScanPeriod(t *testing.T) {
	p := newParkEnv(t, 100*time.Millisecond, false, nil)
	ksid := p.firstTurn()

	parked := parkedTask("t-park", ksid, "ask-1", a2a.TaskStateInputRequired)
	p.srv.Script(kagenttest.Emit{Event: parked})
	p.srv.AddTask(parked)
	p.send("two")

	p.waitCount(1, "a pending request for the parked turn, though ScanEvery is an hour",
		`SELECT count(*) FROM hitl_requests WHERE task_id = 't-park' AND availability = 'pending'`)
}

// An auth-required park is not a HITL ask the observer adopts, so it does not make the session due.
// The input-required park beside it is the control: the same setup does mark the session due.
func TestAnAuthRequiredParkDoesNotHintTheObserver(t *testing.T) {
	for _, tc := range []struct {
		state    a2a.TaskState
		wantHint bool
	}{
		{a2a.TaskStateAuthRequired, false},
		{a2a.TaskStateInputRequired, true},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			// No observer: only the delivery loop runs, so scanned_at changes only through the hint.
			e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ParkedMax = 50 * time.Millisecond })
			r := reconcile.NewRunner(e.pool, quiet(), nil)
			l := e.svc.DeliveryLoop()
			l.Interval = 20 * time.Millisecond
			r.Add(l)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			e.waitState(e.send("one"), dbstate.DeliveryCompleted)
			ksid := e.kagentSessionID()
			if _, err := e.pool.Exec(ctx, `INSERT INTO hitl_sessions
				(kagent_session_id, session_id, owner_id, agent_namespace, agent_name, context_id, scanned_at)
				SELECT $1, session_id, $2, $3, $4, $1, now() FROM agent_bindings WHERE kagent_session_id = $1`,
				ksid, owner, agent.Namespace, agent.Name); err != nil {
				t.Fatal(err)
			}

			parked := parkedTask("t-park", ksid, "ask-1", tc.state)
			e.srv.Script(kagenttest.Emit{Event: parked})
			e.srv.AddTask(parked)
			e.send("two")

			// Let the parked turn read the task several times.
			deadline := time.Now().Add(10 * time.Second)
			for e.srv.Count("GetTask") < 4 {
				if time.Now().After(deadline) {
					t.Fatal("the parked turn was not read again")
				}
				time.Sleep(20 * time.Millisecond)
			}
			var scanned bool
			if err := e.pool.QueryRow(ctx,
				`SELECT scanned_at IS NOT NULL FROM hitl_sessions WHERE kagent_session_id = $1`, ksid).Scan(&scanned); err != nil {
				t.Fatal(err)
			}
			if scanned == tc.wantHint {
				t.Fatalf("scanned_at set = %v after a %s park, want hint = %v", scanned, tc.state, tc.wantHint)
			}
		})
	}
}
