package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// stopUnconfirmed cancels a code child whose runtime stop kagent does not confirm, and waits until
// the resolver has looked at kagent once.
func (r *rig) stopUnconfirmed(sup tasks.Principal, child string) (attempt string) {
	r.t.Helper()
	attempt, _ = r.attemptOf(child)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "c1")); err != nil {
		r.t.Fatal(err)
	}
	r.eventually("stop looked at", func() bool {
		return r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'runtime-stop-unconfirmed' = ANY (evidence_refs)`, attempt) == 1 &&
			r.stops.lookCount() > 0
	})
	return attempt
}

func (r *rig) held(attempt string) (slot, claim int) {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND capacity_held`, attempt),
		r.count(`SELECT count(*) FROM workspace_writer_claims WHERE attempt_id = $1 AND held`, attempt)
}

func TestUnconfirmedStopKeepsTheSlotUntilResolved(t *testing.T) {
	r := newRig(t)
	r.ses.agent = "unknown"
	r.svc.StopObserver = r.stops
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt := r.stopUnconfirmed(sup, child)

	// Held: the agent may still be running, so the attempt is not final and keeps its branch and slot.
	// The task is not final until the stop is: the cancel is still in flight.
	if got := r.taskStatus(child); got == "cancelled" {
		t.Fatal("the task ended cancelled with the stop unconfirmed")
	}
	for range 3 {
		r.sweep(r.svc.SettleSweep())
		time.Sleep(30 * time.Millisecond)
	}
	if got := r.attemptState(child); got != "draining" {
		t.Fatalf("attempt = %s, want draining while the stop is unconfirmed", got)
	}
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d, want both", slot, claim)
	}
	if len(r.ws.removed) != 0 {
		t.Fatalf("workspace removed: %v", r.ws.removed)
	}
	if r.stops.lookCount() < 2 {
		t.Fatalf("looks = %d: the resolver did not retry", r.stops.lookCount())
	}
	// Kagent is asked to stop once; the resolver only observes and cancels through its own client.
	if n := r.ses.cancelCount(); n != 1 {
		t.Fatalf("session cancels = %d", n)
	}

	// Resolved: nothing runs any more.
	r.stops.set(true)
	r.eventually("released", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(child) == "cancelled"
	})
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d after resolution", slot, claim)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'runtime-stop-confirmed' = ANY (evidence_refs)`, attempt); n != 1 {
		t.Fatal("no runtime-stop-confirmed evidence")
	}
	if n := r.ses.cancelCount(); n != 1 {
		t.Fatalf("session cancels = %d after resolution", n)
	}
}

// The reviewer's probe: an unconfirmed stop must not wedge the branch or the parent for good.
func TestProbeUnconfirmedStopWedges(t *testing.T) {
	r := newRig(t)
	r.ses.agent = "unknown"
	r.svc.StopObserver = r.stops
	r.run()
	_, sup := r.supervisor()
	first := r.codeChild(sup, "feat")
	r.eventually("first", func() bool { return r.opState(first) == "completed" })
	r.stopUnconfirmed(sup, first)

	// While the stop is unresolved the branch stays with the first attempt.
	second := r.codeChild(sup, "feat")
	time.Sleep(200 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, second); n != 0 {
		t.Fatalf("second child got an attempt while the branch was held: %d", n)
	}
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "early", dbstate.OutcomeCompleted)); err == nil {
		t.Fatal("the parent completed with a child still open")
	}

	r.stops.set(true)
	r.eventually("second gets the branch", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.opState(second) == "completed" && r.attemptState(second) == "active"
	})
	if g := r.count(`SELECT writer_generation FROM task_attempts WHERE task_id = $1`, second); g != 2 {
		t.Fatalf("generation = %d", g)
	}
	// The second child ends too, and then the parent can complete.
	if _, err := r.svc.Cancel(context.Background(), r.own, action(second, r.view(r.own, second), "c2")); err != nil {
		t.Fatal(err)
	}
	r.eventually("second cancelled", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(second) == "cancelled"
	})
	r.eventually("parent completes", func() bool {
		_, err := r.svc.Report(context.Background(), sup, report(sup, "done", dbstate.OutcomeCompleted))
		return err == nil
	})
}

func TestUnresolvedStopIsRaisedAfterTheBound(t *testing.T) {
	r := newRig(t)
	r.ses.agent = "unknown"
	r.svc.StopObserver = r.stops
	r.svc.Limits.ResolveBound = 3
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt := r.stopUnconfirmed(sup, child)
	r.eventually("unresolved raised", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'runtime-stop-unresolved' = ANY (evidence_refs)`, attempt) == 1
	})
	if got, reason := r.taskStatus(child), r.str(`SELECT reason FROM tasks WHERE id = $1`, child); got != "blocked" || reason != "reconciliation" {
		t.Fatalf("task = %s/%s, want blocked/reconciliation", got, reason)
	}
	// Raised once, and still held: evidence is not a release.
	r.sweep(r.svc.SettleSweep())
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND array_length(array_positions(evidence_refs, 'runtime-stop-unresolved'), 1) > 1`, attempt); n != 0 {
		t.Fatal("the evidence was written twice")
	}
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d", slot, claim)
	}
	// A late confirmation still ends it: the blocked task is cancelled.
	r.stops.set(true)
	r.eventually("released", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(child) == "cancelled" && r.taskStatus(child) == "cancelled"
	})
}

func TestFailedFirstBriefIsSentAgainThenBlocksTheTask(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, session := r.attemptOf(child)
	failBrief := func() {
		r.exec(`UPDATE deliveries SET state = 'failed', detail = 'kagent refused' WHERE id = (SELECT brief_delivery_id FROM task_attempts WHERE id = $1)`, attempt)
		r.sweep(r.svc.BriefSweep())
	}
	// A busy or healthy brief is left alone.
	r.sweep(r.svc.BriefSweep())
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1`, session); n != 1 {
		t.Fatalf("messages = %d", n)
	}
	first := r.str(`SELECT brief_delivery_id FROM task_attempts WHERE id = $1`, attempt)
	failBrief()
	second := r.str(`SELECT brief_delivery_id FROM task_attempts WHERE id = $1`, attempt)
	if second == first {
		t.Fatal("a failed brief was not sent again under a new id")
	}
	failBrief()
	if r.str(`SELECT brief_delivery_id FROM task_attempts WHERE id = $1`, attempt) == second {
		t.Fatal("the second failed brief was not sent again")
	}
	if got := r.taskStatus(child); got != "running" {
		t.Fatalf("child = %s before the bound", got)
	}
	failBrief() // third failure: no more sends
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1`, session); n != 3 {
		t.Fatalf("brief messages = %d, want 3", n)
	}
	if got := r.taskStatus(child); got != "blocked" {
		t.Fatalf("child = %s", got)
	}
	if got := r.str(`SELECT reason FROM tasks WHERE id = $1`, child); got != "reconciliation" {
		t.Fatalf("reason = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'brief-delivery-failed' = ANY (evidence_refs)`, attempt); n != 1 {
		t.Fatal("no brief-delivery-failed evidence")
	}
	// Once blocked, further passes do nothing.
	r.sweep(r.svc.BriefSweep())
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1`, session); n != 3 {
		t.Fatalf("brief messages = %d after the bound", n)
	}
}

func TestPermanentWorkspaceErrorsAbortProvisioning(t *testing.T) {
	for name, err := range map[string]error{
		"invalid":         &workspaces.InvalidError{Msg: "depth must be between 0 and 1000"},
		"unknown project": workspaces.ErrProjectNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.run()
			_, sup := r.supervisor()
			r.ws.fail = err
			child := r.codeChild(sup, "feat")
			r.eventually("failed", func() bool { return r.taskStatus(child) == "failed" && r.attemptState(child) == "failed" })
			if got := r.opState(child); got != "blocked" {
				t.Fatalf("op = %s", got)
			}
			if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child); n != 0 {
				t.Fatalf("capacity held = %d", n)
			}
			if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`); n != 0 {
				t.Fatalf("held claims = %d", n)
			}
		})
	}
}

func TestAbortRacingCancelEndsTheTaskCancelled(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	gate := make(chan struct{})
	r.ws.gate, r.ws.entered = gate, make(chan struct{}, 1)
	r.ws.gateErr = &workspaces.RejectedError{Detail: "kagent refused"}
	child := r.codeChild(sup, "feat")
	select {
	case <-r.ws.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never started")
	}
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "c1")); err != nil {
		t.Fatal(err)
	}
	r.eventually("cancel took the attempt", func() bool { return r.attemptState(child) != "creating" })
	close(gate) // the create now fails: the abort finds the attempt gone
	r.eventually("create op blocked", func() bool { return r.opState(child) == "blocked" })
	r.eventually("cancelled", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(child) == "cancelled" && r.attemptState(child) == "cancelled"
	})
	if got := r.taskStatus(child); got != "cancelled" {
		t.Fatalf("task = %s", got)
	}
}

func TestStoppedParentTurnDoesNotBlockTheChild(t *testing.T) {
	r := newRig(t)
	_, supSession, child, kid := r.reportingChild()
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	r.eventually("first send", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession) == 1
	})
	// The owner presses stop on the parent's turn: the ledger row ends cancelled.
	r.exec(`UPDATE deliveries SET state = 'cancelled' WHERE state = 'recorded' AND id IN (SELECT delivery_id FROM task_event_deliveries)`)
	r.eventually("sent again", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession) == 2
	})
	if got := r.taskStatus(child); got == "blocked" {
		t.Fatal("the child was blocked though its parent is alive")
	}
}
