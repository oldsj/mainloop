package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// reportingChild provisions a supervisor with one active code child and returns the pieces.
func (r *rig) reportingChild() (parent, supSession, child string, kid tasks.Principal) {
	r.t.Helper()
	r.run()
	parent, sup := r.supervisor()
	_, supSession = r.attemptOf(parent)
	r.finishTurns() // the supervisor's own brief turn is over
	child = r.codeChild(sup, "feat")
	r.eventually("child provisioned", func() bool { return r.opState(child) == "completed" })
	_, binding := r.attemptOf(child)
	return parent, supSession, child, r.agent(dbstate.RoleChild, binding)
}

// failOpenReports marks the ledger rows of queued report deliveries failed, as a kagent that
// rejected the message would, and returns how many it changed.
func (r *rig) failOpenReports() int {
	r.t.Helper()
	tag, err := r.pool.Exec(context.Background(), `UPDATE deliveries SET state = 'failed', detail = 'kagent refused'
		WHERE state = 'recorded' AND id IN (SELECT delivery_id FROM task_event_deliveries WHERE state = 'queued')`)
	if err != nil {
		r.t.Fatal(err)
	}
	return int(tag.RowsAffected())
}

func TestFailedDeliveryIsSentAgainThenBlocksTheTask(t *testing.T) {
	r := newRig(t)
	_, supSession, child, kid := r.reportingChild()
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	failures := 0
	r.eventually("delivery uncertain", func() bool {
		failures += r.failOpenReports()
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE state = 'uncertain'`) == 1
	})
	// Three sends, each under its own message id, then no more.
	if failures != 3 {
		t.Fatalf("sends = %d, want 3", failures)
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession); n != 3 {
		t.Fatalf("report messages = %d", n)
	}
	// The failure is evidence on the reporting attempt, and the task waits for review.
	attempt, _ := r.attemptOf(child)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND EXISTS (SELECT 1 FROM unnest(evidence_refs) e WHERE e LIKE 'native-delivery-failed:%: kagent refused')`, attempt); n != 1 {
		t.Fatal("no native-delivery-failed evidence")
	}
	if got := r.taskStatus(child); got != "blocked" {
		t.Fatalf("child status = %s", got)
	}
	if got := r.str(`SELECT reason FROM tasks WHERE id = $1`, child); got != "reconciliation" {
		t.Fatalf("child reason = %s", got)
	}
}

func TestParentEndedAfterTheReportCancelsTheDelivery(t *testing.T) {
	r := newRig(t)
	parent, _, child, kid := r.reportingChild()
	// The parent ends between the report committing and the loop sending it.
	r.exec(`UPDATE task_event_deliveries SET next_attempt_at = now() + interval '1 hour'`)
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE tasks SET status = 'cancelled', reason = NULL, version = version + 1 WHERE id = $1`, parent)
	r.exec(`UPDATE task_event_deliveries SET next_attempt_at = now()`)
	r.eventually("delivery cancelled", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE state = 'cancelled'`) == 1
	})
	if n := r.count(`SELECT count(*) FROM deliveries WHERE id IN (SELECT delivery_id FROM task_event_deliveries)`); n != 0 {
		t.Fatalf("a message was sent to an ended parent: %d", n)
	}
	// The report itself is intact: the child's record does not depend on the recipient.
	if n := r.count(`SELECT count(*) FROM task_reports WHERE task_id = $1`, child); n != 1 {
		t.Fatalf("reports = %d", n)
	}
}

func TestBlockedReportAlsoReachesMain(t *testing.T) {
	r := newRig(t)
	_, supSession, _, kid := r.reportingChild()
	in := report(kid, "b1", dbstate.OutcomeBlocked)
	in.OwnerAction = "decide the schema"
	if _, err := r.svc.Report(context.Background(), kid, in); err != nil {
		t.Fatal(err)
	}
	r.eventually("both deliveries queued", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE state IN ('queued', 'delivered') AND delivery_id IS NOT NULL`) == 2
	})
	var main string
	if err := r.pool.QueryRow(context.Background(), `SELECT session_id FROM agent_bindings WHERE role = 'main'`).Scan(&main); err != nil {
		t.Fatal(err)
	}
	if main == supSession {
		t.Fatal("main and the supervisor share a session")
	}
	got := r.str(`SELECT content FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, main)
	if !strings.Contains(got, "blocked; unverified claim") || !strings.HasSuffix(got, "Owner action needed: decide the schema") {
		t.Fatalf("main message = %q", got)
	}
}

func TestBusyParentGetsTheReportLater(t *testing.T) {
	r := newRig(t)
	r.run()
	parent, sup := r.supervisor()
	_, supSession := r.attemptOf(parent)
	// The supervisor's brief turn is still open: one open turn per session, so the report waits.
	child := r.codeChild(sup, "feat")
	r.eventually("child provisioned", func() bool { return r.opState(child) == "completed" })
	_, binding := r.attemptOf(child)
	kid := r.agent(dbstate.RoleChild, binding)
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "r1", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	r.eventually("attempted", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE attempts > 0`) == 1
	})
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession); n != 0 {
		t.Fatalf("a report was sent into an open turn: %d", n)
	}
	r.finishTurns()
	r.exec(`UPDATE task_event_deliveries SET next_attempt_at = now()`)
	r.eventually("sent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession) == 1
	})
}
