package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// completedCoordination is a root coordination task whose supervisor reported completed, after the
// task completed and its attempt settled: the state a follow-up reopens.
func (r *rig) completedCoordination() (id, attempt, session string) {
	r.t.Helper()
	r.run()
	id, sup := r.supervisor()
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "done", dbstate.OutcomeCompleted)); err != nil {
		r.t.Fatal(err)
	}
	attempt, session = r.attemptOf(id)
	r.eventually("task completed and attempt settled", func() bool {
		r.finishTurns() // the reporting turn, then the report's delivery to main
		r.sweep(r.svc.CompleteSweep())
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "completed" && r.attemptState(id) == "completed"
	})
	return id, attempt, session
}

func followUp(t *testing.T, r *rig, p tasks.Principal, task, req string) tasks.FollowUpResult {
	t.Helper()
	res, err := r.svc.FollowUp(context.Background(), p, tasks.FollowUpInput{TaskID: task, RequestID: req, Text: "and the staging cluster?"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var e *tasks.Error
	if !errors.As(err, &e) || e.Reason != reason {
		t.Fatalf("err = %v, want %s", err, reason)
	}
}

// A follow-up to a completed coordination task reopens it on the same attempt and native session:
// no new attempt, session or binding, and the message is the session's next turn.
func TestFollowUpReopensACompletedTaskOnItsSession(t *testing.T) {
	r := newRig(t)
	id, attempt, session := r.completedCoordination()
	before := r.view(r.own, id).Task.Version

	res := followUp(t, r, r.own, id, "f1")
	if !res.Reopened || res.AttemptID != attempt || res.SessionID != session || res.How != "sent" || res.Version != before+1 {
		t.Fatalf("follow-up = %+v (version before %d)", res, before)
	}
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("task = %s, want running", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, id); n != 1 {
		t.Fatalf("attempts = %d, want the one attempt reopened", n)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND state = 'active' AND stop_state = 'none'
		AND capacity_held AND reopened_at IS NOT NULL`, attempt); n != 1 {
		t.Fatal("attempt was not reopened with a fresh stop fence")
	}
	if got := r.str(`SELECT status FROM sessions WHERE id = $1`, session); got != "active" {
		t.Fatalf("session = %s, want active", got)
	}
	if n := r.count(`SELECT count(*) FROM deliveries WHERE id = $1 AND session_id = $2 AND state = 'recorded'`, res.MessageID, session); n != 1 {
		t.Fatal("the follow-up is not the session's next turn")
	}

	// A retry finds the first send: no second message, no second reopen.
	again := followUp(t, r, r.own, id, "f1")
	if again.MessageID != res.MessageID || again.Reopened {
		t.Fatalf("retry = %+v, first = %+v", again, res)
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content = 'and the staging cluster?'`, session); n != 1 {
		t.Fatalf("messages = %d, want 1 after a retry", n)
	}

	// The reopened attempt authenticates and reports again, and the task completes again.
	sup := r.agent(dbstate.RoleSupervisor, session)
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "done-2", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	r.eventually("completed again", func() bool {
		r.finishTurns()
		r.sweep(r.svc.CompleteSweep())
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "completed" && r.attemptState(id) == "completed"
	})
}

// Main's and a supervisor's follow-ups say who sent them; the owner's is the owner's own words.
func TestFollowUpFromMainIsLabelled(t *testing.T) {
	r := newRig(t)
	id, _, session := r.completedCoordination()
	followUp(t, r, r.mainPrincipal(), id, "m1")
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content = '[message from main]' || chr(10) || 'and the staging cluster?'`, session); n != 1 {
		t.Fatal("main's follow-up is not labelled")
	}
}

// A follow-up to an open task with an active attempt is the session's next message, and puts the
// task back to running: a blocked task was answered, and a task waiting on its completion waits for
// the agent's next report instead.
func TestFollowUpToAnOpenTaskSendsAndResumesIt(t *testing.T) {
	r := newRig(t)
	_, supSession, child, kid := r.reportingChild()
	sup := r.agent(dbstate.RoleSupervisor, supSession)
	_, childSession := r.attemptOf(child)
	for _, outcome := range []dbstate.ReportOutcome{dbstate.OutcomeBlocked, dbstate.OutcomeCompleted} {
		if _, err := r.svc.Report(context.Background(), kid, report(kid, "r-"+string(outcome), outcome)); err != nil {
			t.Fatal(err)
		}
		if got := r.taskStatus(child); got == "running" {
			t.Fatalf("%s report left the task running", outcome)
		}
		r.drainTurns()
		res := followUp(t, r, sup, child, "f-"+string(outcome))
		if res.Reopened || res.How != "sent" || r.taskStatus(child) != "running" {
			t.Fatalf("after %s: follow-up = %+v, task = %s", outcome, res, r.taskStatus(child))
		}
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[message from supervisor task %'`, childSession); n != 2 {
		t.Fatalf("follow-ups = %d", n)
	}
}

// The runtime bound is the owner's to lift: main is refused, the owner's follow-up restarts it.
func TestFollowUpToARuntimeLimitedTaskNeedsTheOwner(t *testing.T) {
	r := newRig(t)
	_, _, child, _ := r.reportingChild()
	attempt, _ := r.attemptOf(child)
	r.exec(`UPDATE tasks SET status = 'blocked', reason = 'runtime_limit', version = version + 1 WHERE id = $1`, child)
	_, err := r.svc.FollowUp(context.Background(), r.mainPrincipal(), tasks.FollowUpInput{TaskID: child, RequestID: "x", Text: "go on"})
	wantReason(t, err, tasks.ReasonRuntimeLimitOwner)
	followUp(t, r, r.own, child, "o1")
	if r.taskStatus(child) != "running" || r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND reopened_at IS NOT NULL`, attempt) != 1 {
		t.Fatal("the owner's follow-up did not restart the runtime bound")
	}
}

func TestFollowUpRefusals(t *testing.T) {
	r := newRig(t)
	parent, supSession, child, kid := r.reportingChild()
	sup := r.agent(dbstate.RoleSupervisor, supSession)
	ctx := context.Background()
	in := func(task string) tasks.FollowUpInput {
		return tasks.FollowUpInput{TaskID: task, RequestID: uuid.NewString(), Text: "t"}
	}

	// A supervisor manages its children, not its own task; a child manages nothing.
	_, err := r.svc.FollowUp(ctx, sup, in(parent))
	wantReason(t, err, tasks.ReasonManagementDenied)
	_, err = r.svc.FollowUp(ctx, kid, in(child))
	wantReason(t, err, tasks.ReasonManagementDenied)

	// A completed code task's branch has merged: more work is a new task.
	r.exec(`UPDATE tasks SET status = 'completed', reason = NULL, version = version + 1 WHERE id = $1`, child)
	_, err = r.svc.FollowUp(ctx, r.own, in(child))
	wantReason(t, err, tasks.ReasonFollowUpCode)

	// An owner's cancel is final, and a failed task is retried.
	for _, st := range []string{"cancelled", "failed"} {
		r.exec(`UPDATE tasks SET status = $2, version = version + 1 WHERE id = $1`, child, st)
		_, err = r.svc.FollowUp(ctx, r.own, in(child))
		wantReason(t, err, tasks.ReasonTaskTerminal)
	}
}

// A completed task whose attempt is still settling is refused for now, not reopened under the
// settle sweep.
func TestFollowUpWaitsForTheAttemptToSettle(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _ := r.supervisor()
	r.exec(`UPDATE tasks SET status = 'completed', reason = NULL, version = version + 1 WHERE id = $1`, id)
	_, err := r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: id, RequestID: "s", Text: "t"})
	wantReason(t, err, tasks.ReasonTaskSettling)
}

// A native session kagent no longer has is refused, not replaced by a new, empty one; an asleep one
// takes the follow-up.
func TestFollowUpChecksTheNativeSession(t *testing.T) {
	r := newRig(t)
	id, attempt, session := r.completedCoordination()
	r.exec(`UPDATE agent_bindings SET kagent_session_id = 'k-gone' WHERE session_id = $1`, session)
	r.live.mu.Lock()
	r.live.live = map[string]kagent.SessionLiveness{"k-gone": {}}
	r.live.mu.Unlock()
	_, err := r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: id, RequestID: "g", Text: "t"})
	wantReason(t, err, tasks.ReasonSessionGone)
	if got := r.attemptState(id); got != "completed" || r.taskStatus(id) != "completed" {
		t.Fatalf("a refused follow-up changed attempt %s to %s", attempt, got)
	}

	r.live.mu.Lock()
	r.live.live = map[string]kagent.SessionLiveness{"k-gone": {Exists: true, Asleep: true}}
	r.live.mu.Unlock()
	if res := followUp(t, r, r.own, id, "a"); !res.Reopened {
		t.Fatalf("asleep session: %+v", res)
	}
}

// An archived session's kagent Session is deleted: the task is not reopened.
func TestFollowUpToAnArchivedSessionIsRefused(t *testing.T) {
	r := newRig(t)
	id, _, session := r.completedCoordination()
	r.exec(`UPDATE sessions SET archived_at = now() WHERE id = $1`, session)
	_, err := r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: id, RequestID: "a", Text: "t"})
	wantReason(t, err, tasks.ReasonSessionArchived)
	if r.taskStatus(id) != "completed" || r.attemptState(id) != "completed" {
		t.Fatal("a refused follow-up changed the task")
	}
}

// Concurrent follow-ups to a completed task reopen it once: one attempt, one reopening, and every
// message on the one session, one open turn and the rest queued behind it.
func TestConcurrentFollowUpsReopenOnce(t *testing.T) {
	for round := range 3 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRig(t)
			id, attempt, session := r.completedCoordination()
			const n = 6
			var wg sync.WaitGroup
			results := make(chan error, n)
			reopened := make(chan bool, n)
			start := make(chan struct{})
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					res, err := r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: id, RequestID: fmt.Sprint("c", i), Text: "t"})
					results <- err
					reopened <- err == nil && res.Reopened
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			close(reopened)
			ok, reopens := 0, 0
			for err := range results {
				var e *tasks.Error
				switch {
				case err == nil:
					ok++
				case errors.As(err, &e) && e.Reason == tasks.ReasonOperationPending:
					// lost a race for the open turn; nothing was written, a retry sends it
				default:
					t.Errorf("follow-up: %v", err)
				}
			}
			for r := range reopened {
				if r {
					reopens++
				}
			}
			if reopens != 1 {
				t.Fatalf("reopens = %d, want 1", reopens)
			}
			if c := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'attempt-reopened:follow-up' = ANY(evidence_refs)`, attempt); c != 1 {
				t.Fatalf("reopen evidence on %d attempts", c)
			}
			if c := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, id); c != 1 {
				t.Fatalf("attempts = %d", c)
			}
			open := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1 AND state IN ('recorded', 'sending', 'delivered')`, session)
			queued := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1 AND state = 'queued'`, session)
			if open != 1 || open+queued != ok {
				t.Fatalf("open = %d, queued = %d, accepted = %d", open, queued, ok)
			}
		})
	}
}

// A follow-up racing a cancel leaves one outcome: the cancel wins whatever the order, and no
// follow-up is sent to a stopped attempt.
func TestFollowUpRacingACancel(t *testing.T) {
	for round := range 4 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRig(t)
			_, _, child, _ := r.reportingChild()
			_, session := r.attemptOf(child)
			r.drainTurns()
			v := r.view(r.own, child)
			var wg sync.WaitGroup
			var cancelErr, followErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, cancelErr = r.svc.Cancel(context.Background(), r.own, action(child, v, "cancel"))
			}()
			go func() {
				defer wg.Done()
				<-start
				_, followErr = r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: child, RequestID: "f", Text: "t"})
			}()
			close(start)
			wg.Wait()
			if followErr != nil {
				var e *tasks.Error
				if !errors.As(followErr, &e) || (e.Reason != tasks.ReasonOperationPending && e.Reason != tasks.ReasonTaskNotRunning) {
					t.Fatalf("follow-up: %v", followErr)
				}
			}
			if cancelErr != nil {
				// The follow-up moved the task first; the owner cancels the task as it is now.
				wantReason(t, cancelErr, tasks.ReasonStaleVersion)
				if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "cancel-2")); err != nil {
					t.Fatal(err)
				}
			}
			r.eventually("cancelled", func() bool {
				r.sweep(r.svc.SettleSweep())
				return r.taskStatus(child) == "cancelled" && r.attemptState(child) == "cancelled"
			})
			if n := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1 AND state IN ('queued', 'recorded', 'sending', 'delivered')`, session); n != 0 {
				t.Fatalf("%d messages still open on a cancelled task", n)
			}
			// A follow-up after the cancel is refused: the cancel is final.
			_, err := r.svc.FollowUp(context.Background(), r.own, tasks.FollowUpInput{TaskID: child, RequestID: "late", Text: "t"})
			wantReason(t, err, tasks.ReasonTaskTerminal)
		})
	}
}

// A follow-up queued behind the agent's last turn runs before the task completes: completion waits
// for the session's pending messages, so the stop that follows never cancels it.
func TestCompletionWaitsForAQueuedFollowUp(t *testing.T) {
	r := newRig(t)
	r.run()
	id, sup := r.supervisor()
	_, session := r.attemptOf(id)
	r.drainTurns()
	r.runningTurn(session) // the agent's turn, still open
	if res := followUp(t, r, r.own, id, "q"); res.How != "queued" {
		t.Fatalf("follow-up = %+v, want queued", res)
	}
	// In that same turn, the agent reports it is done.
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "done", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	// The report reaches main; only the session's own messages are still open.
	r.eventually("report delivered", func() bool {
		r.exec(`UPDATE deliveries SET state = 'completed' WHERE session_id <> $1 AND state IN ('recorded', 'sending', 'delivered')`, session)
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE state IN ('pending', 'queued')`) == 0
	})
	for range 3 {
		r.sweep(r.svc.CompleteSweep())
	}
	if got := r.taskStatus(id); got == "completed" {
		t.Fatal("the task completed with a follow-up still queued")
	}
	r.drainTurns()
	r.eventually("completed", func() bool {
		r.finishTurns()
		r.sweep(r.svc.CompleteSweep())
		return r.taskStatus(id) == "completed"
	})
}
