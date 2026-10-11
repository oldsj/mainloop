package tasks_test

import (
	"context"
	"errors"
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

// The owner presses stop on the parent's turn: the ledger row ends cancelled. That is an owner act,
// so the report is not sent over it, and the child is not blocked for it.
func TestStoppedParentTurnEndsTheDeliveryWithoutBlockingTheChild(t *testing.T) {
	r := newRig(t)
	_, supSession, child, kid := r.reportingChild()
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	r.eventually("first send", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession) == 1
	})
	r.exec(`UPDATE deliveries SET state = 'cancelled', detail = 'stopped by user' WHERE state = 'recorded' AND id IN (SELECT delivery_id FROM task_event_deliveries)`)
	r.eventually("delivery cancelled", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE recipient_key LIKE 'parent-task:%' AND state = 'cancelled'`) == 1
	})
	time.Sleep(100 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession); n != 1 {
		t.Fatalf("report messages = %d: the report was sent over the owner's stop", n)
	}
	if got := r.taskStatus(child); got != "running" {
		t.Fatalf("child = %s, want running", got)
	}
	// The report stays on the child.
	if n := r.count(`SELECT count(*) FROM task_reports WHERE task_id = $1`, child); n != 1 {
		t.Fatalf("reports = %d", n)
	}
}

// The owner presses stop on the child's first turn and steers it: the brief is not sent again.
func TestOwnerStopOfTheBriefTurnLeavesTheAttempt(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, session := r.attemptOf(child)
	r.exec(`UPDATE deliveries SET state = 'cancelled', detail = 'stopped by user' WHERE id = (SELECT brief_delivery_id FROM task_attempts WHERE id = $1)`, attempt)
	r.exec(`UPDATE agent_bindings SET queue_held = true WHERE session_id = $1`, session)
	for range 3 {
		r.sweep(r.svc.BriefSweep())
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND role = 'user'`, session); n != 1 {
		t.Fatalf("brief messages = %d, want the one the owner stopped", n)
	}
	if n := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1 AND state IN ('recorded','sending','delivered')`, session); n != 0 {
		t.Fatalf("open deliveries = %d", n)
	}
	if got := r.str(`SELECT queue_held::text FROM agent_bindings WHERE session_id = $1`, session); got != "true" {
		t.Fatalf("queue_held = %s: the hold was cleared", got)
	}
	if got := r.taskStatus(child); got != "running" || r.attemptState(child) != "active" {
		t.Fatalf("task = %s attempt = %s", got, r.attemptState(child))
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'brief-delivery-failed' = ANY (evidence_refs)`, attempt); n != 0 {
		t.Fatal("brief-delivery-failed evidence for a brief the owner stopped")
	}
}

// A failed brief to a session that has ended gives up with evidence and a block; a busy or not yet
// ready session is tried again.
func TestBriefToAnEndedSessionGivesUp(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, session := r.attemptOf(child)
	r.briefRow(attempt, `state = 'failed', detail = 'kagent refused'`)
	if _, err := r.ses.Service.Cancel(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	r.sweep(r.svc.BriefSweep())
	if got := r.taskStatus(child); got != "blocked" {
		t.Fatalf("task = %s, want blocked", got)
	}
	if got := r.str(`SELECT reason FROM tasks WHERE id = $1`, child); got != "reconciliation" {
		t.Fatalf("reason = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'brief-delivery-failed' = ANY (evidence_refs)`, attempt); n != 1 {
		t.Fatal("no brief-delivery-failed evidence")
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND role = 'user'`, session); n != 1 {
		t.Fatalf("brief messages = %d: a message was recorded to an ended session", n)
	}
}

func TestBriefToANotReadySessionIsTriedAgain(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, session := r.attemptOf(child)
	r.briefRow(attempt, `state = 'failed', detail = 'kagent refused'`)
	r.exec(`UPDATE agent_bindings SET kagent_session_id = NULL WHERE session_id = $1`, session)
	for range 3 {
		r.sweep(r.svc.BriefSweep())
	}
	if got := r.taskStatus(child); got != "running" {
		t.Fatalf("task = %s: a session that is not ready yet was given up on", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'brief-delivery-failed' = ANY (evidence_refs)`, attempt); n != 0 {
		t.Fatal("brief-delivery-failed evidence for a session that is only not ready")
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id = 'ks-' || session_id WHERE session_id = $1`, session)
	r.sweep(r.svc.BriefSweep())
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND role = 'user'`, session); n != 2 {
		t.Fatalf("brief messages = %d, want the resend", n)
	}
}

// A create that fails in a way with no type (a database or transport error) is tried again only
// within the provisioning window.
func TestUntypedCreateErrorsAbortAfterTheWindow(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	r.svc.Limits.ProvisionWindow = 200 * time.Millisecond
	r.ws.fail = errors.New("connection reset by peer")
	child := r.codeChild(sup, "feat")
	r.eventually("failed", func() bool { return r.taskStatus(child) == "failed" && r.attemptState(child) == "failed" })
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child); n != 0 {
		t.Fatalf("capacity held = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`); n != 0 {
		t.Fatalf("held claims = %d", n)
	}
}
