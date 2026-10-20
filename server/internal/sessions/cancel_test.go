package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func TestCancelStopsTasksOnCurrentAndPreviousNativeContexts(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	mid := e.firstDelivery(id)
	e.waitState(mid, dbstate.DeliveryCompleted)
	current := e.bindingKsid(id)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET kagent_session_id = 'old-context' WHERE id = $1`, mid); err != nil {
		t.Fatal(err)
	}
	e.srv.AddTask(runningTask("old-task", "old-context", "old-message", ""))
	e.srv.AddTask(runningTask("current-task", current, "native-message", ""))
	e.srv.AddTask(runningTask("unrelated-task", "unrelated-context", "unrelated-message", ""))
	out, err := e.svc.Cancel(context.Background(), id)
	if err != nil || out.Agent != sessions.AgentStopped {
		t.Fatalf("cancel %+v, %v", out, err)
	}
	for _, tid := range []a2a.TaskID{"old-task", "current-task"} {
		task, _ := e.srv.Task(tid)
		if task.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("%s state %s", tid, task.Status.State)
		}
	}
	if task, _ := e.srv.Task("unrelated-task"); task.Status.State != a2a.TaskStateWorking {
		t.Fatal("cancelled another session's task")
	}
}

type blockedTaskList struct {
	sessions.Kagent
	entered chan struct{}
	release chan struct{}
}

func (k *blockedTaskList) ListTasks(ctx context.Context, agent kagent.AgentRef, contextID string) ([]*a2a.Task, error) {
	close(k.entered)
	select {
	case <-k.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return k.Kagent.ListTasks(ctx, agent, contextID)
}

func TestCancelCommitsBeforeNativeIOAndConcurrentCancelDoesNotReplay(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	e.srv.AddTask(runningTask("native-task", e.bindingKsid(id), "native-message", ""))
	k := &blockedTaskList{Kagent: e.svc.Kagent, entered: make(chan struct{}), release: make(chan struct{})}
	e.svc.Kagent = k
	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		out, err := e.svc.Cancel(runCtx, id)
		if err == nil && out.Agent != sessions.AgentStopped {
			t.Errorf("first cancel %+v", out)
		}
		done <- err
	}()
	defer func() {
		close(k.release)
		if err := <-done; err != nil {
			t.Errorf("first cancel: %v", err)
		}
	}()
	select {
	case <-k.entered:
	case <-runCtx.Done():
		t.Fatal("cancel did not reach the native listing")
	}
	row, err := dbq.New(e.pool).GetSessionRow(runCtx, dbq.GetSessionRowParams{ID: id, UserID: owner})
	if err != nil || row.Status != dbstate.SessionCancelled || row.AgentCancelOutcome == nil || *row.AgentCancelOutcome != sessions.AgentUnknown {
		t.Fatalf("committed intent %+v, %v", row, err)
	}
	// The send lock must be available while ListTasks waits on the network.
	tx, err := e.pool.Begin(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.XactLock(runCtx, tx, "mainloop:send:"+id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if err := tx.Rollback(runCtx); err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.Cancel(runCtx, id)
	if err != nil || second.Agent != sessions.AgentUnknown || e.srv.Count("CancelTask") != 0 {
		t.Fatalf("concurrent cancel %+v, %v", second, err)
	}
}

func TestCancelAnUnresolvedSendRemainsUnknown(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := uuid.NewString()
	// A lost send reply whose native task is not visible yet cannot prove nothing is running.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO deliveries (id, idempotency_key, session_id, state, kagent_session_id, next_attempt_at)
		VALUES ($1, $1, $2, 'uncertain', $3, now() + interval '1 hour')`, mid, id, e.bindingKsid(id)); err != nil {
		t.Fatal(err)
	}
	out, err := e.svc.Cancel(context.Background(), id)
	if err != nil || out.Agent != sessions.AgentUnknown {
		t.Fatalf("cancel %+v, %v", out, err)
	}
	if d := e.delivery(mid); d.State != dbstate.DeliveryCancelled {
		t.Fatalf("delivery state %s", d.State)
	}
}
