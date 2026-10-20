package schedules_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

func scheduledExecutionID(t *testing.T, e *env, sid string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(ctx, `SELECT execution_id FROM scheduled_run_links WHERE session_id = $1`, sid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func reconcileCancel(t *testing.T, e *env, svc *sessions.Service, sid string) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, `UPDATE sessions SET agent_cancel_next_attempt_at = now() WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	r := reconcile.NewRunner(e.pool, svc.Log, nil)
	if ran, err := r.RunSweep(ctx, svc.CancelSweep()); !ran || err != nil {
		t.Fatalf("sweep ran=%v: %v", ran, err)
	}
}

func assertCancelOutcome(t *testing.T, svc *sessions.Service, sid, want string) {
	t.Helper()
	got, err := svc.Cancel(ctx, sid)
	if err != nil || got.Status != "cancelled" || got.Agent != want {
		t.Fatalf("cancel=%+v, err=%v; want %s", got, err, want)
	}
}

func TestReviewS2CancelBeforeScheduledTaskAppears(t *testing.T) {
	e, svc, _, sid, ksid := adoptedSession(t)
	// RUNNING was committed, but the A2A send has not exposed a task yet.
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	e.srv.AddTask(&a2a.Task{ID: "late-scheduled-turn", ContextID: ksid,
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	if e.srv.Count("CancelTask") != 0 {
		t.Fatal("repeat cancel replayed native I/O")
	}
	// Even cancellation of the late task is not evidence about the scheduled send until
	// the execution identifies its task. The sweep must keep looking after this pass.
	reconcileCancel(t, e, svc, sid)
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	task, _ := e.srv.Task("late-scheduled-turn")
	if task.Status.State != a2a.TaskStateCanceled {
		t.Fatal("sweep did not cancel the late native task")
	}
	e.srv.EditExecution(scheduledExecutionID(t, e, sid), func(x *kagentapi.ScheduledRunExecution) { x.TaskId = "late-scheduled-turn" })
	// A fresh Service models restart: reconciliation depends on durable state alone.
	restarted := &sessions.Service{Pool: svc.Pool, OwnerID: svc.OwnerID, Kagent: svc.Kagent, Log: svc.Log}
	reconcileCancel(t, e, restarted, sid)
	assertCancelOutcome(t, restarted, sid, sessions.AgentStopped)
	if e.srv.Count("CancelTask") != 1 || e.srv.Count("SendStreamingMessage") != 0 {
		t.Fatal("reconciliation repeated a stopped task or sent a prompt")
	}
}

func TestCancelReservedScheduledSessionWaitsForDispatch(t *testing.T) {
	e, svc, _, sid, ksid := adoptedSession(t)
	executionID := scheduledExecutionID(t, e, sid)
	reserve(e, executionID, ksid, kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_PENDING)
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	// A long delay and repeated empty listings never prove the reservation cannot dispatch.
	for i := 0; i < 7; i++ {
		reconcileCancel(t, e, svc, sid)
		assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
		var attempts int
		var delay float64
		if err := e.pool.QueryRow(ctx, `SELECT agent_cancel_attempts,
			extract(epoch FROM agent_cancel_next_attempt_at - now()) FROM sessions WHERE id = $1`, sid).Scan(&attempts, &delay); err != nil {
			t.Fatal(err)
		}
		wantDelay := min(30*time.Second*time.Duration(1<<min(i, 4)), 5*time.Minute).Seconds()
		if attempts != min(i+1, 5) || delay < wantDelay-5 || delay > wantDelay {
			t.Fatalf("attempt %d: attempts=%d delay=%f; want %f", i, attempts, delay, wantDelay)
		}
		reads := e.srv.Count(kagent.ProcGetScheduledRunExecution)
		if err := svc.ReconcileScheduledCancels(ctx); err != nil {
			t.Fatal(err)
		}
		if e.srv.Count(kagent.ProcGetScheduledRunExecution) != reads {
			t.Fatal("backoff did not prevent an immediate retry")
		}
	}
	e.srv.AddTask(&a2a.Task{ID: "reserved-turn", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	e.srv.EditExecution(executionID, func(x *kagentapi.ScheduledRunExecution) { x.State, x.TaskId = running, "reserved-turn" })
	reconcileCancel(t, e, svc, sid)
	assertCancelOutcome(t, svc, sid, sessions.AgentStopped)
	task, _ := e.srv.Task("reserved-turn")
	if task.Status.State != a2a.TaskStateCanceled {
		t.Fatal("reserved dispatch escaped cancellation")
	}
}

func TestScheduledCancelFinalExecutionRelistsAndCancelsTasks(t *testing.T) {
	for _, lateTask := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-dispatch", true: "late-task"}[lateTask], func(t *testing.T) {
			e, svc, _, sid, ksid := adoptedSession(t)
			assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
			reserve(e, scheduledExecutionID(t, e, sid), ksid, failedSt)
			want := sessions.AgentNotRunning
			if lateTask {
				e.srv.AddTask(&a2a.Task{ID: "late-task", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
				want = sessions.AgentStopped
			}
			reconcileCancel(t, e, svc, sid)
			assertCancelOutcome(t, svc, sid, want)
			if lateTask {
				task, _ := e.srv.Task("late-task")
				if task.Status.State != a2a.TaskStateCanceled {
					t.Fatal("final execution hid a running native task")
				}
			}
			reads := e.srv.Count(kagent.ProcGetScheduledRunExecution)
			reconcileCancel(t, e, svc, sid)
			if e.srv.Count(kagent.ProcGetScheduledRunExecution) != reads {
				t.Fatal("settled cancellation was revisited")
			}
		})
	}
}

func TestScheduledCancelReadAndCancelFailuresRemainUnknown(t *testing.T) {
	for _, operation := range []string{kagent.ProcGetScheduledRunExecution, "ListTasks", "CancelTask"} {
		t.Run(operation, func(t *testing.T) {
			e, svc, _, sid, ksid := adoptedSession(t)
			assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
			e.srv.AddTask(&a2a.Task{ID: "scheduled-task", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
			e.srv.EditExecution(scheduledExecutionID(t, e, sid), func(x *kagentapi.ScheduledRunExecution) { x.TaskId = "scheduled-task" })
			e.srv.FailNext(operation, kagenttest.Fault{Status: 500})
			reconcileCancel(t, e, svc, sid)
			assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
			reconcileCancel(t, e, svc, sid)
			assertCancelOutcome(t, svc, sid, sessions.AgentStopped)
		})
	}
}

type scheduledCancelProbe struct {
	*kagent.Client
	t         *testing.T
	e         *env
	sid       string
	execution *kagentapi.ScheduledRunExecution
	afterList func()
}

func (p *scheduledCancelProbe) ListTasks(ctx context.Context, agent kagent.AgentRef, id string) ([]*a2a.Task, error) {
	tasks, err := p.Client.ListTasks(ctx, agent, id)
	if p.afterList != nil {
		f := p.afterList
		p.afterList = nil
		f()
	}
	return tasks, err
}

func (p *scheduledCancelProbe) GetScheduledRunExecution(ctx context.Context, id string) (*kagentapi.ScheduledRunExecution, error) {
	// Network I/O must see committed cancellation and retry pacing. It must also be able to
	// acquire the row lock from another connection: the sweep cannot hold a transaction here.
	tx, err := p.e.pool.Begin(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 AND status = 'cancelled'
		AND agent_cancel_outcome = 'unknown' AND agent_cancel_attempts > 0 FOR UPDATE NOWAIT`, p.sid).Scan(&locked); err != nil {
		p.t.Fatal(err)
	}
	if p.execution != nil {
		return p.execution, nil
	}
	return p.Client.GetScheduledRunExecution(ctx, id)
}

func TestScheduledCancelReconcileCommitsBeforeNativeIOAndRejectsWrongExecution(t *testing.T) {
	e, svc, client, sid, ksid := adoptedSession(t)
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	id := scheduledExecutionID(t, e, sid)
	probe := &scheduledCancelProbe{Client: client, t: t, e: e, sid: sid}
	svc.Kagent = probe
	for _, mismatch := range []string{"id", "schedule", "session"} {
		x, err := client.GetScheduledRunExecution(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		x.State = succeeded
		switch mismatch {
		case "id":
			x.Id = "foreign-execution"
		case "schedule":
			x.ScheduledRunId = "foreign-schedule"
		case "session":
			x.SessionId = "foreign-context"
		}
		probe.execution = x
		reconcileCancel(t, e, svc, sid)
		assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	}
	probe.execution = nil
	reserve(e, id, ksid, succeeded)
	reconcileCancel(t, e, svc, sid)
	assertCancelOutcome(t, svc, sid, sessions.AgentNotRunning)
}

func TestScheduledCancelReconcileRetainsUnresolvedMainloopSend(t *testing.T) {
	e, svc, _, sid, ksid := adoptedSession(t)
	if _, err := e.pool.Exec(ctx, `INSERT INTO deliveries (id, idempotency_key, session_id, state, kagent_session_id)
		VALUES ('unresolved-followup', 'unresolved-followup', $1, 'uncertain', $2)`, sid, ksid); err != nil {
		t.Fatal(err)
	}
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	reserve(e, scheduledExecutionID(t, e, sid), ksid, succeeded)
	reconcileCancel(t, e, svc, sid)
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
}

func TestScheduledCancelRelistsAfterItsTaskBecomesFinal(t *testing.T) {
	e, svc, client, sid, ksid := adoptedSession(t)
	assertCancelOutcome(t, svc, sid, sessions.AgentUnknown)
	e.srv.EditExecution(scheduledExecutionID(t, e, sid), func(x *kagentapi.ScheduledRunExecution) { x.TaskId = "original-task" })
	e.srv.AddTask(&a2a.Task{ID: "original-task", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	svc.Kagent = &scheduledCancelProbe{Client: client, t: t, e: e, sid: sid, afterList: func() {
		e.srv.AddTask(&a2a.Task{ID: "appeared-during-list", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	}}
	reads := e.srv.Count("ListTasks")
	reconcileCancel(t, e, svc, sid)
	assertCancelOutcome(t, svc, sid, sessions.AgentStopped)
	for _, id := range []a2a.TaskID{"original-task", "appeared-during-list"} {
		task, _ := e.srv.Task(id)
		if task.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("%s escaped cancellation", id)
		}
	}
	if e.srv.Count("ListTasks")-reads != 2 || e.srv.Count("CancelTask") != 2 {
		t.Fatal("missing the listing after scheduled-task finality")
	}
}
