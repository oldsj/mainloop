package sessions_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func TestReadySessionResumesASuspendedSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil {
		t.Fatal(err)
	}
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	// Ready already: nothing more to do.
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil || e.srv.Count(kagent.ProcResumeSession) != 1 {
		t.Fatalf("ready session: %v, %d resumes", err, e.srv.Count(kagent.ProcResumeSession))
	}
}

// wakeFake stands in for the workspace service: waking the workspace resumes its Session.
type wakeFake struct {
	resume func() error
	wakes  atomic.Int32
}

func (w *wakeFake) Wake(context.Context, string) error {
	w.wakes.Add(1)
	return w.resume()
}

func TestReadySessionWakesTheWorkspaceBehindASuspendedSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	w := &wakeFake{resume: func() error {
		_, err := e.other().ResumeSession(context.Background(), ksid)
		return err
	}}
	e.svc.Waker = w
	ws := "ws1"
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err != nil {
		t.Fatal(err)
	}
	if w.wakes.Load() != 1 {
		t.Fatalf("%d wakes", w.wakes.Load())
	}
	// A workspace is resumed through its own wake, never directly.
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes (the wake's own)", n)
	}
}

func TestReadySessionFailsWhenTheWorkspaceStaysAsleep(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 100 * time.Millisecond })
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	e.svc.Waker = &wakeFake{resume: func() error { return nil }} // wakes, but the Session stays suspended
	ws := "ws1"
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err == nil {
		t.Fatal("a Session that is still suspended after the wake is not ready")
	}
	if err := e.svc.ReadySession(context.Background(), "ws1", nil, ""); err == nil {
		t.Fatal("no kagent session is not ready")
	}
}

// kagent admits nothing to a prepared-workspace Session until it confirms the preparation, and
// words the refusal as a bad reply; a send waits for it, and a failed preparation is final.
func TestReadySessionWaitsForWorkspacePreparation(t *testing.T) {
	t.Parallel()
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
