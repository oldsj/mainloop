package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func taskLoop(req, task string, statuses ...dbstate.TaskStatus) tasks.LoopInput {
	return tasks.LoopInput{RequestID: req, Trigger: dbstate.LoopTriggerTask, TaskID: task, Statuses: statuses, Note: "look at " + task}
}

func (r *rig) arm(p tasks.Principal, in tasks.LoopInput) string {
	r.t.Helper()
	l, err := r.svc.Arm(context.Background(), p, in)
	if err != nil {
		r.t.Fatal(err)
	}
	return l.ID
}

func (r *rig) loopState(id string) string {
	return r.str(`SELECT state FROM task_loops WHERE id = $1`, id)
}

// A supervisor's loop on its child fires when the child reaches a status it named, and the firing
// goes out with the child's own report as one message once the supervisor's turn ends.
func TestLoopFiresWithTheReportInOneMessage(t *testing.T) {
	r := newRig(t)
	_, supSession, child, kid := r.reportingChild()
	loop := r.arm(r.agentOf(supSession), taskLoop("l1", child, dbstate.TaskBlocked, dbstate.TaskFailed))
	r.openTurn(supSession, uuid.NewString())
	in := report(kid, "b1", dbstate.OutcomeBlocked)
	in.OwnerAction = "pick a schema"
	if _, err := r.svc.Report(context.Background(), kid, in); err != nil {
		t.Fatal(err)
	}
	r.eventually("the loop fired", func() bool {
		return r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop) == 1
	})
	r.eventually("report and firing waiting", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE recipient_key LIKE 'parent-task:%' AND state = 'pending' AND attempts > 0`) == 2
	})
	r.exec(`UPDATE task_event_deliveries SET next_attempt_at = now() + interval '200 milliseconds' WHERE state = 'pending'`)
	r.finishTurns()
	r.eventually("one message to the supervisor", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[2 updates]%'`, supSession) == 1
	})
	got := r.str(`SELECT content FROM session_messages WHERE session_id = $1 AND content LIKE '[2 updates]%'`, supSession)
	if !strings.Contains(got, "[report from child task") || !strings.Contains(got, "[loop "+loop+" fired: firing 1 of at most 20]") ||
		!strings.Contains(got, "You asked: look at "+child) || !strings.Contains(got, "is blocked/owner_decision, was running") {
		t.Fatalf("message = %q", got)
	}
	if got := r.loopState(loop); got != "armed" {
		t.Fatalf("loop state = %s, want armed", got)
	}
}

// agentOf is the supervisor principal of a session.
func (r *rig) agentOf(session string) tasks.Principal {
	r.t.Helper()
	return r.agent(dbstate.RoleSupervisor, session)
}

// Arming after the change still hears of it: a subject already in a wanted status fires at once.
// A change to a status the loop did not ask for does not fire.
func TestLoopOnAStatusAlreadyReachedFiresAtOnce(t *testing.T) {
	r := newRig(t)
	_, supSession, child, _ := r.reportingChild()
	sup := r.agentOf(supSession)
	r.setStatus(child, "blocked", "owner_decision")
	at := r.arm(sup, taskLoop("now", child, dbstate.TaskBlocked))
	quiet := r.arm(sup, taskLoop("later", child, dbstate.TaskCompleted))
	r.eventually("the first loop fired", func() bool {
		return r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, at) == 1
	})
	r.setStatus(child, "running", "")
	r.eventually("both loops saw running", func() bool {
		return r.count(`SELECT count(*) FROM task_loops WHERE baseline = 'running'`) == 2
	})
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, quiet); n != 0 {
		t.Fatalf("a loop on completed fired %d times", n)
	}
}

// Occurrences while the firing waits fold into it: one message, every fact in order.
func TestOccurrencesWhileAFiringWaitsFoldIntoIt(t *testing.T) {
	r := newRig(t)
	_, supSession, child, _ := r.reportingChild()
	loop := r.arm(r.agentOf(supSession), taskLoop("any", child))
	r.openTurn(supSession, uuid.NewString())
	for _, st := range []string{"waiting", "running", "waiting"} {
		r.setStatus(child, st, map[string]string{"waiting": "handoff", "running": ""}[st])
		r.eventually("seen "+st, func() bool {
			return r.str(`SELECT baseline FROM task_loops WHERE id = $1`, loop) == map[string]string{"waiting": "waiting/handoff", "running": "running"}[st]
		})
	}
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop); n != 1 {
		t.Fatalf("firings = %d, want 1", n)
	}
	if n := r.count(`SELECT cardinality(facts) FROM task_loop_firings WHERE loop_id = $1`, loop); n != 3 {
		t.Fatalf("facts = %d, want 3", n)
	}
	r.finishTurns()
	r.eventually("sent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[loop %'`, supSession) == 1
	})
	got := r.str(`SELECT content FROM session_messages WHERE session_id = $1 AND content LIKE '[loop %'`, supSession)
	i1 := strings.Index(got, "is waiting/handoff, was running")
	i2 := strings.Index(got, "is running, was waiting/handoff")
	i3 := strings.LastIndex(got, "is waiting/handoff, was running")
	if i1 < 0 || i2 < i1 || i3 < i2 {
		t.Fatalf("message = %q, want the three changes in order", got)
	}
	if n := r.count(`SELECT firings FROM task_loops WHERE id = $1`, loop); n != 1 {
		t.Fatalf("loop firings = %d, want 1: folded occurrences do not count", n)
	}
}

// A time loop fires when due and again every interval, each firing after the minimum interval, and
// stops at max_firings.
func TestTimeLoopRepeatsUntilItsLimit(t *testing.T) {
	r := newRig(t)
	r.run()
	main := r.mainPrincipal()
	zero := time.Duration(0)
	l, err := r.svc.Arm(context.Background(), main, tasks.LoopInput{
		RequestID: "tick", Trigger: dbstate.LoopTriggerTime, EveryMinutes: 30, Note: "check prod", MaxFirings: 2, MinInterval: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.FireAt == nil || time.Until(*l.FireAt) < 29*time.Minute {
		t.Fatalf("fire_at = %v, want 30 minutes away", l.FireAt)
	}
	due := func() { r.exec(`UPDATE task_loops SET fire_at = now(), next_attempt_at = now() WHERE id = $1`, l.ID) }
	due()
	r.eventually("first firing", func() bool { return r.count(`SELECT firings FROM task_loops WHERE id = $1`, l.ID) == 1 })
	if got := r.loopState(l.ID); got != "armed" {
		t.Fatalf("state = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM task_loops WHERE id = $1 AND fire_at > now() + interval '29 minutes'`, l.ID); n != 1 {
		t.Fatal("the next occurrence is not an interval away")
	}
	r.eventually("first message sent", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE recipient_key = 'main' AND state IN ('queued', 'delivered')`) == 1
	})
	r.finishTurns()
	due()
	r.eventually("exhausted", func() bool { return r.loopState(l.ID) == "exhausted" })
	r.eventually("second message sent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE content LIKE '[loop %This was the loop''s last firing (max_firings).'`) == 1
	})
}

// A firing waits min_interval after the one before it.
func TestFiringsKeepTheirMinimumInterval(t *testing.T) {
	r := newRig(t)
	r.run()
	main := r.mainPrincipal()
	l := r.arm(main, tasks.LoopInput{RequestID: "tick", Trigger: dbstate.LoopTriggerTime, EveryMinutes: 1, Note: "n"})
	due := func() { r.exec(`UPDATE task_loops SET fire_at = now(), next_attempt_at = now() WHERE id = $1`, l) }
	due()
	r.eventually("first message sent", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE loop_firing_id IS NOT NULL AND state = 'queued'`) == 1
	})
	r.finishTurns()
	due()
	r.eventually("second firing", func() bool { return r.count(`SELECT firings FROM task_loops WHERE id = $1`, l) == 2 })
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1 AND number = 2
		AND not_before >= (SELECT not_before FROM task_loop_firings WHERE loop_id = $1 AND number = 1) + interval '60 seconds'`, l); n != 1 {
		t.Fatal("the second firing is not held for the minimum interval")
	}
	time.Sleep(100 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM task_event_deliveries d JOIN task_loop_firings f ON f.id = d.loop_firing_id
		WHERE f.number = 2 AND d.delivery_id IS NULL`); n != 1 {
		t.Fatal("the second firing was sent inside the minimum interval")
	}
}

// A one-time timer is done after it fires.
func TestOneTimeTimerIsDone(t *testing.T) {
	r := newRig(t)
	r.run()
	l := r.arm(r.mainPrincipal(), tasks.LoopInput{RequestID: "once", Trigger: dbstate.LoopTriggerTime, AfterMinutes: 5, Note: "n"})
	r.exec(`UPDATE task_loops SET fire_at = now(), next_attempt_at = now() WHERE id = $1`, l)
	r.eventually("done", func() bool { return r.loopState(l) == "done" })
	r.eventually("sent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE content LIKE '[loop %This one-time timer is done.'`) == 1
	})
}

// A supervisor's loops end with its task, and an expired loop stops; neither fires again.
func TestLoopsEndWithTheirRecipientAndExpire(t *testing.T) {
	r := newRig(t)
	parent, supSession, child, _ := r.reportingChild()
	sup := r.agentOf(supSession)
	loop := r.arm(sup, taskLoop("l1", child))
	other := r.arm(r.mainPrincipal(), taskLoop("l2", child))
	r.exec(`UPDATE task_loops SET expires_at = now() WHERE id = $1`, other)
	r.setStatus(parent, "cancelled", "")
	r.setStatus(child, "cancelled", "")
	r.eventually("ended", func() bool { return r.loopState(loop) == "ended" })
	r.eventually("expired", func() bool { return r.loopState(other) == "expired" })
	if n := r.count(`SELECT count(*) FROM task_loop_firings`); n != 0 {
		t.Fatalf("firings = %d", n)
	}
}

// Arming is idempotent on the request id, bounded per recipient, and limited to the tasks the
// caller manages; children do not arm loops. A stopped loop no longer fires.
func TestArmingRules(t *testing.T) {
	r := newRig(t)
	parent, supSession, child, kid := r.reportingChild()
	sup := r.agentOf(supSession)
	ctx := context.Background()
	first := r.arm(sup, taskLoop("same", child))
	if again := r.arm(sup, taskLoop("same", child)); again != first {
		t.Fatalf("a retried arm made a second loop: %s, %s", first, again)
	}
	changed := taskLoop("same", child)
	changed.Note = "something else"
	_, err := r.svc.Arm(ctx, sup, changed)
	wantReason(t, err, tasks.ReasonPayloadConflict)
	_, err = r.svc.Arm(ctx, sup, taskLoop("own", parent))
	wantReason(t, err, tasks.ReasonLoopSubjectDenied)
	_, err = r.svc.Arm(ctx, kid, tasks.LoopInput{RequestID: "kid", Trigger: dbstate.LoopTriggerTime, AfterMinutes: 1, Note: "n"})
	wantReason(t, err, tasks.ReasonManagementDenied)
	_, err = r.svc.Arm(ctx, sup, tasks.LoopInput{RequestID: "bad", Trigger: dbstate.LoopTriggerTask, Note: "n"})
	wantReason(t, err, tasks.ReasonInvalidRequest)
	for i := 1; i < tasks.MaxArmedLoops; i++ {
		r.arm(sup, tasks.LoopInput{RequestID: uuid.NewString(), Trigger: dbstate.LoopTriggerTime, AfterMinutes: 60, Note: "n"})
	}
	_, err = r.svc.Arm(ctx, sup, tasks.LoopInput{RequestID: "one-more", Trigger: dbstate.LoopTriggerTime, AfterMinutes: 60, Note: "n"})
	wantReason(t, err, tasks.ReasonLoopLimit)

	stopped, err := r.svc.StopLoop(ctx, sup, first)
	if err != nil || stopped.State != dbstate.LoopStopped {
		t.Fatalf("stop = %+v, %v", stopped.State, err)
	}
	// Another recipient's loop is not found.
	_, err = r.svc.StopLoop(ctx, r.mainPrincipal(), first)
	wantReason(t, err, tasks.ReasonLoopNotFound)
	r.setStatus(child, "blocked", "owner_decision")
	time.Sleep(100 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, first); n != 0 {
		t.Fatal("a stopped loop fired")
	}
	ls, err := r.svc.Loops(ctx, sup)
	if err != nil || len(ls) != tasks.MaxArmedLoops || ls[len(ls)-1].ID != first {
		t.Fatalf("loops = %d, %v; want the armed ones, then the stopped one", len(ls), err)
	}
}

// A supervisor that armed a loop and ended its turn is waiting, not stalled.
func TestLivenessDoesNotReadASupervisorWaitingOnALoopAsStalled(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now().Add(-50*time.Minute))
	r.exec(`INSERT INTO task_loops (id, owner_id, principal_key, request_id, recipient_key, trigger, note,
		max_firings, min_interval_seconds, expires_at, fire_at)
		VALUES ('l-1', $1, 'k', 'r', 'parent-task:' || $2, 'time', 'n', 1, 0, now() + interval '1 hour', now() + interval '1 hour')`, owner, id)
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running while the loop is armed", got)
	}
	r.exec(`UPDATE task_loops SET state = 'stopped' WHERE id = 'l-1'`)
	r.sweep(r.svc.LivenessSweep())
	if got, reason := r.taskStatus(id), r.taskReason(id); got != "blocked" || reason != "stalled" {
		t.Fatalf("after stop: status = %s reason = %s, want blocked stalled", got, reason)
	}
}

// projectPR sets what the task projection saw of a task's pull request.
func (r *rig) projectPR(task string, number int, state, ci, ciHead, head string) {
	r.t.Helper()
	r.exec(`UPDATE tasks SET pr_number = $2::int, pr_url = 'https://github.com/o/p/pull/' || $2::text, pr_head_sha = $6,
		pr_state = $3, ci_state = $4, ci_head_sha = NULLIF($5, '') WHERE id = $1`, task, number, state, ci, ciHead, head)
}

// A pull_request loop on ci_success waits out CI at an older commit and pending CI at the head,
// then fires once CI is green at the head; the message says how the pull request stands.
func TestPullRequestLoopFiresWhenCIIsGreenAtTheHead(t *testing.T) {
	r := newRig(t)
	_, supSession, child, _ := r.reportingChild()
	loop := r.arm(r.agentOf(supSession), tasks.LoopInput{
		RequestID: "green", Trigger: dbstate.LoopTriggerPullRequest, TaskID: child,
		PRConditions: []dbstate.PRCondition{dbstate.PRCISuccess, dbstate.PRMerged}, Note: "merge it",
	})
	head := strings.Repeat("b", 40)
	r.projectPR(child, 9, "open", "success", strings.Repeat("a", 40), head)
	r.eventually("seen the stale CI", func() bool {
		return strings.Contains(r.str(`SELECT baseline FROM task_loops WHERE id = $1`, loop), "aaaa")
	})
	r.projectPR(child, 9, "open", "pending", head, head)
	r.eventually("seen pending CI", func() bool {
		return strings.Contains(r.str(`SELECT baseline FROM task_loops WHERE id = $1`, loop), "pending")
	})
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop); n != 0 {
		t.Fatalf("fired %d times before CI was green at the head", n)
	}
	r.projectPR(child, 9, "open", "success", head, head)
	r.eventually("sent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[loop %'`, supSession) == 1
	})
	got := r.str(`SELECT content FROM session_messages WHERE session_id = $1 AND content LIKE '[loop %'`, supSession)
	if !strings.Contains(got, "You asked: merge it") || !strings.Contains(got, "pull request #9 is open, CI success at the head bbbbbbbbbbbb") {
		t.Fatalf("message = %q", got)
	}
}

// A condition that already holds when the loop is armed fires at once; a pull_request loop needs a
// code task.
func TestPullRequestLoopOnAConditionThatHoldsFiresAtOnce(t *testing.T) {
	r := newRig(t)
	_, supSession, child, _ := r.reportingChild()
	sup := r.agentOf(supSession)
	r.projectPR(child, 4, "merged", "success", "c", "c")
	loop := r.arm(sup, tasks.LoopInput{
		RequestID: "m", Trigger: dbstate.LoopTriggerPullRequest, TaskID: child,
		PRConditions: []dbstate.PRCondition{dbstate.PRMerged}, Note: "confirm prod",
	})
	r.eventually("fired", func() bool {
		return r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop) == 1
	})
	if got := r.str(`SELECT facts[1] FROM task_loop_firings WHERE loop_id = $1`, loop); !strings.Contains(got, "pull request #4 is merged") {
		t.Fatalf("fact = %q", got)
	}
	coord := r.mustCreate(sup, tasks.CreateInput{})
	_, err := r.svc.Arm(context.Background(), r.mainPrincipal(), tasks.LoopInput{
		RequestID: "c", Trigger: dbstate.LoopTriggerPullRequest, TaskID: coord, Note: "n",
	})
	wantReason(t, err, tasks.ReasonLoopNeedsCodeTask)
}
