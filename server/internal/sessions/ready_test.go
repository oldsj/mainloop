package sessions_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// wakeFake stands in for the workspace service's admission of a send.
type wakeFake struct {
	wake  func() error
	wakes atomic.Int32
}

func (w *wakeFake) Wake(context.Context, string) error {
	w.wakes.Add(1)
	return w.wake()
}

// A sleeping Session is READY: nothing resumes it before a send, which is what wakes its Actor.
func TestReadySessionLeavesASleepingSessionForTheSendToWake(t *testing.T) {
	e := newEnv(t)
	e.srv.Actors = true
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	e.srv.Sleep(ksid)
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil {
		t.Fatal(err)
	}
	if n := e.srv.Wakes(ksid); n != 0 {
		t.Fatalf("readiness woke the Session %d times", n)
	}
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	if n := e.srv.Wakes(ksid); n != 1 || e.kagentSessionID() != ksid || e.srv.Count(kagent.ProcCreateSession) != 1 {
		t.Fatalf("%d wakes; the session was replaced or not woken by the send", n)
	}
}

// A workspace session is admitted through the workspace service, whose refusal (a workspace being
// deleted) stops the send.
func TestReadySessionAsksTheWorkspaceServiceAndHonoursItsRefusal(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	w := &wakeFake{wake: func() error { return nil }}
	e.svc.Waker = w
	ws := "ws1"
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err != nil || w.wakes.Load() != 1 {
		t.Fatalf("%v, %d wakes", err, w.wakes.Load())
	}
	e.svc.Waker = &wakeFake{wake: func() error { return errors.New("the workspace is being deleted") }}
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err == nil {
		t.Fatal("a refused workspace was ready")
	}
	if err := e.svc.ReadySession(context.Background(), "ws1", nil, ""); err == nil {
		t.Fatal("no kagent session is not ready")
	}
}

// kagent's own lifecycle work is brief: a Session it is settling is waited for, not resumed.
func TestReadySessionWaitsOutKagentSettling(t *testing.T) {
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 2 * time.Second })
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	var reads atomic.Int32
	e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcGetSession && reads.Add(1) <= 2 {
			s.State = kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED
			s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME
		}
	}
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil {
		t.Fatal(err)
	}
	if reads.Load() < 3 {
		t.Fatalf("%d reads: a settling Session was treated as ready", reads.Load())
	}
}

// kagent admits nothing to a prepared-workspace Session until it confirms the preparation, and
// words the refusal as a bad reply; a send waits for it, and a failed preparation is final.
func TestReadySessionWaitsForWorkspacePreparation(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO sessions (id,user_id,title,description,prompt) SELECT 'ws1',user_id,title,description,prompt FROM sessions LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ('p1','u','o','n','o/n','https://github.com/o/n')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id,user_id,project_id,repo_url,branch,agent_kind,agent_namespace,agent_name,request_id)
	    VALUES ('ws1','u','p1','https://github.com/o/n','b','claude','kagent','a','req1')`); err != nil {
		t.Fatal(err)
	}
	ws := "ws1"
	// No preparation row: a workspace made without an environment has nothing to wait for.
	if err := e.svc.ReadySession(ctx, "ws1", &ws, ksid); err != nil {
		t.Fatalf("no preparation: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_preparations (id,idempotency_key,action_id) VALUES ('ws1','k','prep:req1')`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{dbstate.PreparationPlanned, dbstate.PreparationRequested} {
		if _, err := e.pool.Exec(ctx, `UPDATE workspace_preparations SET state=$1`, state); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.ReadySession(ctx, "ws1", &ws, ksid); err == nil || !strings.Contains(err.Error(), "not prepared") {
			t.Fatalf("%s: %v", state, err)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_preparations SET state='failed', detail='the setup failed'`); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReadySession(ctx, "ws1", &ws, ksid); err == nil || !strings.Contains(err.Error(), "replace the session") || !strings.Contains(err.Error(), "the setup failed") {
		t.Fatalf("failed: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_preparations SET state='confirmed'`); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReadySession(ctx, "ws1", &ws, ksid); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
}
