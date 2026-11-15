package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// handoffOp is the state of the task's latest retry or reassign.
func (r *rig) handoffOp(id string) string {
	return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind IN ('retry', 'reassign')
		ORDER BY created_at DESC, id DESC LIMIT 1`, id)
}

// firstMessage is the first message the session received: an attempt's brief.
func (r *rig) firstMessage(session string) string {
	return r.str(`SELECT content FROM session_messages WHERE session_id = $1 ORDER BY created_at, id LIMIT 1`, session)
}

// handedOff waits until the task's handoff completed and its successor runs.
func (r *rig) handedOff(id string) {
	r.t.Helper()
	r.eventually("handoff completed", func() bool {
		return r.handoffOp(id) == "completed" && r.taskStatus(id) == "running"
	})
}

// A retry stops the running attempt and starts a successor in a new session on the same provider:
// the old agent loses MCP at once, the old attempt is superseded and linked to its successor, a
// supervisor's open children end with it, and the successor's brief carries a continuation note.
func TestRetryReplacesTheAttempt(t *testing.T) {
	r := newRig(t)
	stop := r.run()
	id, sup := r.supervisor()
	child := r.mustCreate(sup, tasks.CreateInput{Brief: "look into it"})
	oldAttempt, oldSession := r.attemptOf(id)
	// No runner while the handoff is checked in flight: one would make the successor, and a second
	// request on the view read before that is then stale_task_attempt, not operation_pending.
	stop()
	v := r.view(r.own, id)
	if !v.Actions["retry"].Available || !v.Actions["reassign"].Available {
		t.Fatalf("a running coordination task offers no handoff: %+v", v.Actions)
	}

	op, err := r.svc.Retry(context.Background(), r.own, action(id, v, "retry-1"))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "draining" || op.SourceAttemptId == nil || *op.SourceAttemptId != oldAttempt {
		t.Fatalf("op = %+v", op)
	}
	// The request drained the attempt: the old agent cannot report any more.
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "late", dbstate.OutcomeCompleted)); err == nil {
		t.Fatal("the old attempt reported after the retry")
	}
	if got := r.taskStatus(id) + "/" + r.taskReason(id); got != "waiting/handoff" {
		t.Fatalf("task = %s, want waiting/handoff", got)
	}
	if r.view(r.own, id).Actions["retry"].Available {
		t.Fatal("retry is offered again during the handoff")
	}
	_, err = r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "retry-2"))
	wantReason(t, err, tasks.ReasonOperationPending)
	if got := r.taskStatus(child); got != "cancelled" {
		t.Fatalf("child = %s, want cancelled with its supervisor", got)
	}

	r.run()
	r.handedOff(id)
	newAttempt, newSession := r.attemptOf(id)
	if newAttempt == oldAttempt || newSession == oldSession {
		t.Fatal("the retry did not make a new attempt and session")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND state = 'superseded' AND successor_id = $2
		AND superseded_at IS NOT NULL AND NOT capacity_held`, oldAttempt, newAttempt); n != 1 {
		t.Fatal("the old attempt is not superseded by the new one")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND state = 'active' AND predecessor_id = $2
		AND number = 2 AND profile_id = 'claude'`, newAttempt, oldAttempt); n != 1 {
		t.Fatal("the new attempt does not follow the old one on the same provider")
	}
	if got := r.str(`SELECT status FROM sessions WHERE id = $1`, oldSession); got != "cancelled" {
		t.Fatalf("old session = %s, want cancelled", got)
	}
	if n := r.count(`SELECT count(*) FROM credentials WHERE binding_id = $1 AND state IN ('planned', 'published')`, oldSession); n != 0 {
		t.Fatal("the old attempt kept a live credential")
	}
	brief := r.firstMessage(newSession)
	for _, want := range []string{"Ship the thing", "## Continuation", "This task was retried.", "Attempt 1 was stopped and you are attempt 2",
		"Child tasks it had open were cancelled", "It made no report."} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}

	// The successor works like any attempt: it reports and the task completes. The cancelled child
	// gives its slot up once its own cancel settles; until then the supervisor has a live child.
	r.eventually("child settled", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child) == 0
	})
	next := r.agent(dbstate.RoleSupervisor, newSession)
	if _, err := r.svc.Report(context.Background(), next, report(next, "done", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	r.eventually("task completed", func() bool {
		r.finishTurns()
		r.sweep(r.svc.CompleteSweep())
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "completed" && r.attemptState(id) == "completed"
	})
	// A follow-up reopens the successor, never the superseded attempt.
	res := followUp(t, r, r.own, id, "f1")
	if !res.Reopened || res.AttemptID != newAttempt {
		t.Fatalf("follow-up = %+v", res)
	}
}

// A failed task reopens for a retry: the operation goes straight to the successor, whose brief
// quotes the failed attempt's last report.
func TestRetryOfAFailedTaskReopensIt(t *testing.T) {
	r := newRig(t)
	r.run()
	id, sup := r.supervisor()
	oldAttempt, _ := r.attemptOf(id)
	failed := report(sup, "gave-up", dbstate.OutcomeFailed)
	failed.Summary = "the API kept timing out"
	if _, err := r.svc.Report(context.Background(), sup, failed); err != nil {
		t.Fatal(err)
	}
	// A failed report blocks the task for the owner; the task fails by other paths (an aborted
	// provisioning, a liveness bound), which this stands in for.
	r.setStatus(id, "failed", "")
	r.eventually("task failed and attempt settled", func() bool {
		r.finishTurns()
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "failed" && r.attemptState(id) == "failed"
	})
	v := r.view(r.own, id)
	if !v.Actions["retry"].Available || v.Actions["follow_up"].Available {
		t.Fatalf("a failed task's actions = %+v", v.Actions)
	}
	op, err := r.svc.Retry(context.Background(), r.own, action(id, v, "retry-1"))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "requested" {
		t.Fatalf("op = %+v", op)
	}
	r.handedOff(id)
	newAttempt, newSession := r.attemptOf(id)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND state = 'failed' AND successor_id = $2`, oldAttempt, newAttempt); n != 1 {
		t.Fatal("the failed attempt is not linked to its successor")
	}
	if brief := r.firstMessage(newSession); !strings.Contains(brief, "Its last report (failed):\nthe API kept timing out") {
		t.Fatalf("brief does not quote the last report:\n%s", brief)
	}
}

// A reassign moves the task to the target provider; the model and effort go back to its defaults.
func TestReassignMovesTheTaskToTheTarget(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _ := r.supervisor()
	in := action(id, r.view(r.own, id), "move-1")
	in.TargetProfileID = "codex"
	if _, err := r.svc.Reassign(context.Background(), r.own, in); err != nil {
		t.Fatal(err)
	}
	r.handedOff(id)
	attempt, session := r.attemptOf(id)
	if got := r.str(`SELECT assigned_profile_id FROM tasks WHERE id = $1`, id); got != "codex" {
		t.Fatalf("task provider = %s, want codex", got)
	}
	if got := r.str(`SELECT profile_id FROM task_attempts WHERE id = $1`, attempt); got != "codex" {
		t.Fatalf("attempt provider = %s, want codex", got)
	}
	if brief := r.firstMessage(session); !strings.Contains(brief, "This task was reassigned from provider claude.") {
		t.Fatalf("brief does not say where the task came from:\n%s", brief)
	}
}

// Main and supervisors may hand a task off MaxHandoffs times; then only the owner can.
func TestHandoffLimitLeavesTheRestToTheOwner(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxHandoffs = 1
	r.run()
	id, _ := r.supervisor()
	main := r.mainPrincipal()
	if _, err := r.svc.Retry(context.Background(), main, action(id, r.view(r.own, id), "m1")); err != nil {
		t.Fatal(err)
	}
	r.handedOff(id)
	_, err := r.svc.Retry(context.Background(), main, action(id, r.view(r.own, id), "m2"))
	wantReason(t, err, tasks.ReasonHandoffLimit)
	if _, err := r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "o1")); err != nil {
		t.Fatalf("the owner's retry was refused: %v", err)
	}
	r.handedOff(id)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, id); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

// Retries and reassigns of one task at once make one successor: the first takes the task's version
// and the rest are refused, and nothing runs twice.
func TestConcurrentRetriesMakeOneSuccessor(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _ := r.supervisor()
	v := r.view(r.own, id)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := action(id, v, fmt.Sprintf("r%d", i))
			if i%2 == 1 {
				in.TargetProfileID = "codex"
				_, errs[i] = r.svc.Reassign(context.Background(), r.own, in)
				return
			}
			_, errs[i] = r.svc.Retry(context.Background(), r.own, in)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		var e *tasks.Error
		switch {
		case err == nil:
			won++
		case errors.As(err, &e) && e.Status == 409:
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d requests won, want 1: %v", won, errs)
	}
	r.handedOff(id)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, id); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind IN ('retry', 'reassign')`, id); n != 1 {
		t.Fatalf("handoff operations = %d, want 1", n)
	}
}

// An owner's cancel during a handoff wins: whichever step the handoff reached, the task ends
// cancelled and no attempt is left running.
func TestCancelDuringAHandoffWins(t *testing.T) {
	for round := range 3 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRig(t)
			r.run()
			id, _ := r.supervisor()
			if _, err := r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "retry")); err != nil {
				t.Fatal(err)
			}
			// Cancel at a different point of the handoff each round: while the old attempt drains, once
			// the successor exists, and after the handoff completed.
			switch round {
			case 1:
				r.eventually("successor made", func() bool {
					return r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, id) == 2
				})
			case 2:
				r.handedOff(id)
			}
			// The handoff moves the task on while the cancel is sent; a client re-reads and sends again.
			for try := 0; ; try++ {
				_, err := r.svc.Cancel(context.Background(), r.own, action(id, r.view(r.own, id), fmt.Sprintf("cancel-%d", try)))
				var e *tasks.Error
				if err == nil {
					break
				}
				if !errors.As(err, &e) || (e.Reason != tasks.ReasonStaleVersion && e.Reason != tasks.ReasonStaleAttempt) || try == 20 {
					t.Fatal(err)
				}
			}
			r.eventually("task cancelled with nothing running", func() bool {
				r.sweep(r.svc.SettleSweep())
				return r.taskStatus(id) == "cancelled" &&
					r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND state IN ('creating', 'active', 'draining', 'fenced')`, id) == 0 &&
					r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND state NOT IN ('completed', 'blocked')`, id) == 0
			})
		})
	}
}
