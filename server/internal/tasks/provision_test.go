package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// codeChild is a code task of the supervisor on its own branch.
func (r *rig) codeChild(sup tasks.Principal, branch string) string {
	r.t.Helper()
	return r.mustCreate(sup, tasks.CreateInput{Mode: dbstate.TaskModeCode, Checkout: &tasks.Checkout{Branch: branch}})
}

// supervisor creates a root coordination task and waits until its supervisor is active and briefed.
func (r *rig) supervisor() (root string, sup tasks.Principal) {
	r.t.Helper()
	root = r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", Brief: "Ship the thing"})
	r.eventually("supervisor provisioned", func() bool { return r.opState(root) == "completed" })
	_, binding := r.attemptOf(root)
	return root, r.agent(dbstate.RoleSupervisor, binding)
}

func TestDelegateProvisionsAndReports(t *testing.T) {
	r := newRig(t)
	r.run()
	root, sup := r.supervisor()

	// The supervisor: an active attempt with a session and binding, the brief in the ledger with
	// the role guidance in front, and the stored brief as written.
	v := r.view(r.own, root)
	if v.Task.Status != "running" || len(v.Attempts) != 1 || v.Attempts[0].State != "active" || v.Attempts[0].Role != "supervisor" {
		t.Fatalf("supervisor task = %+v attempts = %+v", v.Task, v.Attempts)
	}
	supAttempt, supSession := r.attemptOf(root)
	if got := r.str(`SELECT brief FROM tasks WHERE id = $1`, root); got != "Ship the thing" {
		t.Fatalf("stored brief = %q", got)
	}
	msg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, supSession)
	if !strings.HasPrefix(msg, "You are the Mainloop task supervisor.") || !strings.HasSuffix(msg, "Ship the thing") ||
		!strings.Contains(msg, "no repository authority") {
		t.Fatalf("supervisor brief = %q", msg)
	}
	if got := r.str(`SELECT brief_delivery_id FROM task_attempts WHERE id = $1`, supAttempt); got != tasks.BriefMessageID(supAttempt) {
		t.Fatalf("brief delivery = %q", got)
	}
	if n := r.count(`SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND role = 'supervisor' AND mcp_grant_kind = 'coordination'`, supSession); n != 1 {
		t.Fatalf("supervisor bindings = %d", n)
	}

	// A code child: one workspace, its session as the attempt's, writer generation 1.
	child := r.codeChild(sup, "feat")
	r.eventually("child provisioned", func() bool { return r.opState(child) == "completed" })
	if r.ws.madeCount() != 1 {
		t.Fatalf("workspaces made = %d", r.ws.madeCount())
	}
	cv := r.view(r.own, child)
	if cv.Task.Status != "running" || cv.Attempts[0].State != "active" || cv.Attempts[0].Role != "child" {
		t.Fatalf("child = %+v attempts = %+v", cv.Task, cv.Attempts)
	}
	childAttempt, childBinding := r.attemptOf(child)
	if childBinding != r.ws.made[0] {
		t.Fatalf("child binding = %s, workspace = %s", childBinding, r.ws.made[0])
	}
	if got := r.count(`SELECT writer_generation FROM task_attempts WHERE id = $1`, childAttempt); got != 1 {
		t.Fatalf("writer generation = %d", got)
	}
	if got := r.str(`SELECT role FROM agent_bindings WHERE session_id = $1`, childBinding); got != "child" {
		t.Fatalf("child binding role = %q", got)
	}
	cmsg := r.str(`SELECT content FROM session_messages WHERE session_id = $1`, childBinding)
	if !strings.Contains(cmsg, "open_pull_request") || !strings.Contains(cmsg, "## Workspace environment") {
		t.Fatalf("child brief = %q", cmsg)
	}

	// The child reports; the parent's session receives it once its own turn is over.
	kid := r.agent(dbstate.RoleChild, childBinding)
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "done", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	r.finishTurns()
	r.eventually("report sent to the parent", func() bool {
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[report from child task %'`, supSession) == 1
	})
	got := r.str(`SELECT content FROM session_messages WHERE session_id = $1 AND content LIKE '[report from%'`, supSession)
	if !strings.HasPrefix(got, "[report from child task "+child+" attempt 1; completed; unverified claim]\ndid a thing") {
		t.Fatalf("report text = %q", got)
	}
	r.finishTurns()
	r.eventually("delivery done", func() bool {
		return r.count(`SELECT count(*) FROM task_event_deliveries WHERE state = 'delivered'`) == 1
	})
	// Sent once, however many times the loop looked.
	if n := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1`, supSession); n != 2 {
		t.Fatalf("supervisor ledger rows = %d, want brief + report", n)
	}
}

func TestProvisionReplaysWithoutASecondWorkspace(t *testing.T) {
	r := newRig(t)
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
	stop := r.run()
	r.eventually("supervisor", func() bool { return r.opState(root) == "completed" })
	_, binding := r.attemptOf(root)
	sup := r.agent(dbstate.RoleSupervisor, binding)

	// The first create writes the workspace and then fails, as a process dying before it could
	// attach the workspace would. The replay finds that workspace.
	r.ws.crash = 1
	child := r.codeChild(sup, "feat")
	r.eventually("child provisioned", func() bool { return r.opState(child) == "completed" })
	if r.ws.madeCount() != 1 {
		t.Fatalf("workspaces made = %d, want 1", r.ws.madeCount())
	}
	stop()
	// A restarted process finds everything done and changes nothing.
	r.run()
	time.Sleep(100 * time.Millisecond)
	if r.ws.madeCount() != 1 || r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, child) != 1 {
		t.Fatal("a restart provisioned again")
	}
}

func TestBriefIsNotSentTwice(t *testing.T) {
	r := newRig(t)
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
	r.run()
	r.eventually("supervisor", func() bool { return r.opState(root) == "completed" })
	attempt, session := r.attemptOf(root)

	// Put the operation back to the step before the brief was noted: the ledger has the message, the
	// operation does not know. The replay must not send another.
	r.exec(`UPDATE task_operations SET state = 'target_ready', last_confirmed_step = 'target_ready' WHERE task_id = $1`, root)
	r.exec(`UPDATE task_attempts SET brief_delivery_id = NULL WHERE id = $1`, attempt)
	r.eventually("replayed", func() bool { return r.opState(root) == "completed" })
	if n := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1`, session); n != 1 {
		t.Fatalf("ledger rows = %d", n)
	}
	if got := r.str(`SELECT brief_delivery_id FROM task_attempts WHERE id = $1`, attempt); got != tasks.BriefMessageID(attempt) {
		t.Fatalf("brief delivery = %q", got)
	}
}

func TestLeaseLossReplaysEachStep(t *testing.T) {
	r := newRig(t)
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
	// Run the loop long enough for each step, then stop it and look at the rows as a crash leaves
	// them; the next runner takes the rows over (its own lease owner) and finishes.
	for i := 0; i < 4 && r.opState(root) != "completed"; i++ {
		stop := r.run()
		time.Sleep(15 * time.Millisecond)
		stop()
	}
	r.run()
	r.eventually("supervisor", func() bool { return r.opState(root) == "completed" })
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, root); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM sessions WHERE id IN (SELECT session_id FROM task_attempts WHERE task_id = $1)`, root); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
	_, session := r.attemptOf(root)
	if n := r.count(`SELECT count(*) FROM deliveries WHERE session_id = $1`, session); n != 1 {
		t.Fatalf("briefs = %d", n)
	}
}

func TestCancelMidProvisionLeavesNothing(t *testing.T) {
	r := newRig(t)
	r.ws.unready = true // the workspace never gets its kagent Session: the attempt stays creating
	stop := r.run()
	root, sup := func() (string, tasks.Principal) {
		root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
		r.eventually("supervisor", func() bool { return r.opState(root) == "completed" })
		_, b := r.attemptOf(root)
		return root, r.agent(dbstate.RoleSupervisor, b)
	}()
	_ = root
	child := r.codeChild(sup, "feat")
	r.eventually("workspace made", func() bool { return r.ws.madeCount() == 1 && r.attemptState(child) == "creating" })
	stop()

	cv := r.view(r.own, child)
	op, err := r.svc.Cancel(context.Background(), r.own, action(child, cv, "c1"))
	if err != nil || op.State != "draining" {
		t.Fatalf("cancel = %+v, %v", op, err)
	}
	r.run()
	r.eventually("cancelled", func() bool { return r.taskStatus(child) == "cancelled" && r.attemptState(child) == "cancelled" })

	if n := r.count(`SELECT count(*) FROM workspaces`); n != 0 {
		t.Fatalf("workspaces left = %d removed=%v evidence=%q brief=%q ws=%q", n, r.ws.removed, r.str(`SELECT array_to_string(evidence_refs, ',') FROM task_attempts WHERE task_id = $1`, child), r.str(`SELECT brief_delivery_id FROM task_attempts WHERE task_id = $1`, child), r.str(`SELECT workspace_id FROM task_attempts WHERE task_id = $1`, child))
	}
	if len(r.ws.removed) != 1 || r.ws.removed[0] != r.ws.made[0] {
		t.Fatalf("removed = %v made = %v", r.ws.removed, r.ws.made)
	}
	if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`); n != 0 {
		t.Fatalf("held claims = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child); n != 0 {
		t.Fatalf("capacity held = %d", n)
	}
	if got := r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'create'`, child); got != "blocked" {
		t.Fatalf("create op = %s %s attempt=%s", got, r.str(`SELECT coalesce(target_attempt_id,'NULL') FROM task_operations WHERE task_id = $1 AND kind = 'create'`, child), r.attemptState(child))
	}
	if got := r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'cancel'`, child); got != "completed" {
		t.Fatalf("cancel op = %s", got)
	}
	// Nothing runs a second time after the cancel.
	time.Sleep(100 * time.Millisecond)
	if r.ws.madeCount() != 1 {
		t.Fatalf("workspaces made = %d", r.ws.madeCount())
	}
}

func TestCancelBeforeProvisioningCreatesNothing(t *testing.T) {
	r := newRig(t)
	id := r.mustCreate(r.own, tasks.CreateInput{})
	op, err := r.svc.Cancel(context.Background(), r.own, action(id, r.view(r.own, id), "c1"))
	if err != nil || op.State != "completed" {
		t.Fatalf("cancel = %+v, %v", op, err)
	}
	r.run()
	r.eventually("create op blocked", func() bool { return r.opState(id) == "blocked" })
	time.Sleep(100 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM task_attempts`); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	if v := r.view(r.own, id); v.Task.Status != "cancelled" {
		t.Fatalf("task = %+v", v.Task)
	}
}

func TestCancelledByTheParentBeforeProvisioning(t *testing.T) {
	r := newRig(t)
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
	r.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	r.setStatus(root, "running", "")
	sup := r.agent(dbstate.RoleSupervisor, "sup-1")
	child := r.mustCreate(sup, tasks.CreateInput{})
	// The parent ends before the child's create runs: the child never starts.
	r.setStatus(root, "cancelled", "")
	r.run()
	r.eventually("child blocked", func() bool { return r.opState(child) == "blocked" })
	if got := r.taskStatus(child); got != "cancelled" {
		t.Fatalf("child = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, child); n != 0 {
		t.Fatalf("child attempts = %d", n)
	}
}

func TestCapacityRefusals(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenPerParent, r.svc.Limits.MaxChildrenGlobal = 2, 3
	mk := func(binding string) (string, tasks.Principal) {
		root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
		r.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, binding)
		r.setStatus(root, "running", "")
		return root, r.agent(dbstate.RoleSupervisor, binding)
	}
	_, a := mk("sup-a")
	_, b := mk("sup-b")
	r.mustCreate(a, tasks.CreateInput{})
	r.mustCreate(a, tasks.CreateInput{})
	_, err := r.create(a, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonChildCapacity)
	r.mustCreate(b, tasks.CreateInput{})
	_, err = r.create(b, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonGlobalCapacity)
	if n := r.count(`SELECT count(*) FROM tasks WHERE parent_task_id IS NOT NULL`); n != 3 {
		t.Fatalf("children = %d", n)
	}

	// An ended child frees its slot once its attempt has stopped, not before.
	var ended string
	if err := r.pool.QueryRow(context.Background(), `SELECT id FROM tasks WHERE parent_task_id IS NOT NULL ORDER BY created_at LIMIT 1`).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	att := r.attempt(ended, dbstate.AttemptChild, dbstate.AttemptActive, "kid-1")
	r.setStatus(ended, "cancelled", "")
	_, err = r.create(b, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonGlobalCapacity)
	r.exec(`UPDATE task_attempts SET state = 'draining' WHERE id = $1`, att)
	r.exec(`UPDATE task_attempts SET state = 'fenced', capacity_held = false WHERE id = $1`, att)
	r.mustCreate(b, tasks.CreateInput{})
}

func TestWriterClaimsGoUpByOne(t *testing.T) {
	r := newRig(t)
	stop := r.run()
	_, sup := r.supervisor()
	first := r.codeChild(sup, "feat")
	r.eventually("first", func() bool { return r.opState(first) == "completed" })
	second := r.codeChild(sup, "feat") // the same branch
	// The branch has a writer: the second waits, with no attempt and no workspace.
	time.Sleep(150 * time.Millisecond)
	if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, second) != 0 || r.ws.madeCount() != 1 {
		t.Fatalf("second started while the branch had a writer")
	}
	if got := r.opState(second); got != "requested" {
		t.Fatalf("second op = %s", got)
	}
	stop()

	// The first ends; the claim moves to the second, one generation up.
	if _, err := r.svc.Cancel(context.Background(), r.own, action(first, r.view(r.own, first), "c1")); err != nil {
		t.Fatal(err)
	}
	r.run()
	r.eventually("second provisioned", func() bool { return r.opState(second) == "completed" })
	att, _ := r.attemptOf(second)
	if g := r.count(`SELECT writer_generation FROM task_attempts WHERE id = $1`, att); g != 2 {
		t.Fatalf("generation = %d, want 2", g)
	}
	if g := r.count(`SELECT generation FROM workspace_writer_claims WHERE attempt_id = $1 AND held`, att); g != 2 {
		t.Fatalf("claim generation = %d", g)
	}
}

func TestBranchWaitEndsInFailure(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.ProvisionWindow = time.Millisecond
	r.run()
	_, sup := r.supervisor()
	first := r.codeChild(sup, "feat")
	r.eventually("first", func() bool { return r.opState(first) == "completed" })
	second := r.codeChild(sup, "feat")
	r.eventually("second failed", func() bool { return r.taskStatus(second) == "failed" })
	if got := r.opState(second); got != "blocked" {
		t.Fatalf("second op = %s", got)
	}
}

func TestRejectedWorkspaceAbortsProvisioning(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	r.ws.reject = "no such repository"
	child := r.codeChild(sup, "feat")
	r.eventually("failed", func() bool { return r.taskStatus(child) == "failed" && r.attemptState(child) == "failed" })
	if got := r.opState(child); got != "blocked" {
		t.Fatalf("op = %s", got)
	}
	if got := r.str(`SELECT reason FROM task_operations WHERE task_id = $1`, child); got != "provisioning_unavailable" {
		t.Fatalf("reason = %q", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child); n != 0 {
		t.Fatalf("capacity held = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`); n != 0 {
		t.Fatalf("held claims = %d", n)
	}
	// The branch can be written again: the claim was released, so a retry elsewhere gets generation 2.
	r.ws.reject = ""
	again := r.codeChild(sup, "feat")
	r.eventually("again", func() bool { return r.opState(again) == "completed" })
	att, _ := r.attemptOf(again)
	if g := r.count(`SELECT writer_generation FROM task_attempts WHERE id = $1`, att); g != 2 {
		t.Fatalf("generation = %d", g)
	}
}

func TestSlowWorkspaceSessionAborts(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	r.svc.Limits.ProvisionWindow = 50 * time.Millisecond
	r.ws.unready = true
	child := r.codeChild(sup, "feat")
	r.eventually("failed", func() bool { return r.taskStatus(child) == "failed" && r.attemptState(child) == "failed" })
	if len(r.ws.removed) != 1 {
		t.Fatalf("workspaces removed = %v", r.ws.removed)
	}
	if n := r.count(`SELECT count(*) FROM workspaces`); n != 0 {
		t.Fatalf("workspaces left = %d removed=%v evidence=%q brief=%q ws=%q", n, r.ws.removed, r.str(`SELECT array_to_string(evidence_refs, ',') FROM task_attempts WHERE task_id = $1`, child), r.str(`SELECT brief_delivery_id FROM task_attempts WHERE task_id = $1`, child), r.str(`SELECT workspace_id FROM task_attempts WHERE task_id = $1`, child))
	}
}

func TestCancelledSupervisorCancelsChildren(t *testing.T) {
	r := newRig(t)
	r.run()
	root, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	if _, err := r.svc.Cancel(context.Background(), r.own, action(root, r.view(r.own, root), "c1")); err != nil {
		t.Fatal(err)
	}
	r.eventually("root cancelled", func() bool { return r.taskStatus(root) == "cancelled" })
	r.eventually("child cancelled", func() bool { return r.taskStatus(child) == "cancelled" })
	// The child's agent is stopped by the sweep, which also frees its slot and branch.
	r.eventually("child attempt stopped", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(child) == "cancelled"
	})
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE capacity_held`); n != 0 {
		t.Fatalf("capacity held = %d", n)
	}
}
