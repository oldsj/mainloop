package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func supervisorSend(t *testing.T, r *rig, p tasks.Principal, req, text string) tasks.FollowUpResult {
	t.Helper()
	res, err := r.svc.SupervisorSend(context.Background(), p, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: req, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSupervisorSendStartsTheSupervisorAndSendsAfterItsBrief(t *testing.T) {
	r := newRig(t)
	r.run()
	main := r.mainPrincipal()

	// Given a project with no standing supervisor
	// When main sends it work
	res := supervisorSend(t, r, main, "s-1", "fix the flaky test")

	// Then the project's standing supervisor exists, and the message went to its session after its brief
	id := r.str(`SELECT id FROM tasks WHERE project_id = 'p1' AND standing`)
	_, session := r.attemptOf(id)
	if res.TaskID != id || res.SessionID != session {
		t.Fatalf("result = %+v, supervisor %s session %s", res, id, session)
	}
	if first := r.firstMessage(session); first == "" || first == "[message from main]\nfix the flaky test" {
		t.Fatalf("first message = %q, want the brief", first)
	}
	msg := `SELECT count(*) FROM session_messages WHERE session_id = $1 AND content = '[message from main]' || chr(10) || 'fix the flaky test'`
	if n := r.count(msg, session); n != 1 {
		t.Fatalf("main's messages = %d", n)
	}

	// When main sends it again under the same request id (a lost reply), Then nothing is sent twice
	if again := supervisorSend(t, r, main, "s-1", "fix the flaky test"); again.MessageID != res.MessageID {
		t.Fatalf("retry = %+v, first %+v", again, res)
	}
	if n := r.count(msg, session); n != 1 {
		t.Fatalf("main's messages after the retry = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND standing`); n != 1 {
		t.Fatalf("standing supervisors = %d", n)
	}
}

func TestSupervisorSendWakesASupervisorAtRest(t *testing.T) {
	r := newRig(t)
	r.run()
	main := r.mainPrincipal()
	supervisorSend(t, r, main, "s-1", "first batch")
	id := r.str(`SELECT id FROM tasks WHERE project_id = 'p1' AND standing`)

	// Given the supervisor finished its batch and is at rest
	r.finishTurns()
	r.finishTurns()
	r.setStatus(id, "waiting", "idle")

	// When main sends the next batch
	res := supervisorSend(t, r, main, "s-2", "second batch")

	// Then it is running again on the same task and session
	if got := r.taskStatus(id); got != "running" || res.TaskID != id {
		t.Fatalf("status = %s, result %+v", got, res)
	}
}

func TestSupervisorSendWaitsOutAStartThenRefuses(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.SupervisorStartWait = 50 * time.Millisecond

	// Given no reconciler to give the new supervisor its brief
	// When main sends it work
	_, err := r.svc.SupervisorSend(context.Background(), r.mainPrincipal(), tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-1", Text: "go"})

	// Then it is refused as starting, the supervisor exists, and nothing was sent
	wantRefusal(t, err, 409, tasks.ReasonSupervisorStarting)
	if n := r.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND standing`); n != 1 {
		t.Fatalf("standing supervisors = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE content LIKE '[message from main]%'`); n != 0 {
		t.Fatalf("sent %d messages", n)
	}
}

func TestOnlyMainSendsToASupervisor(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	for _, p := range []tasks.Principal{r.own, sup} {
		_, err := r.svc.SupervisorSend(context.Background(), p, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-1", Text: "go"})
		wantRefusal(t, err, 403, tasks.ReasonManagementDenied)
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE standing`); n != 0 {
		t.Fatalf("a refused send started a supervisor")
	}
}

func TestMainDoesNotDelegateInAProject(t *testing.T) {
	e := newEnv(t)
	main := tasks.Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: "main-1"}

	// When main delegates in a project, in either mode, Then it is refused: the project's supervisor takes it
	for _, mode := range []dbstate.TaskMode{dbstate.TaskModeCode, dbstate.TaskModeCoordination} {
		in := tasks.CreateInput{RequestID: "d-" + string(mode), ProjectID: "p1", Mode: mode}
		if mode == dbstate.TaskModeCode {
			in.Checkout = &tasks.Checkout{Branch: "fix"}
		}
		_, err := e.create(main, in)
		wantRefusal(t, err, 409, tasks.ReasonProjectSupervisor)
	}
	if n := e.count(`SELECT count(*) FROM tasks`); n != 0 {
		t.Fatalf("tasks = %d", n)
	}

	// And outside any project it still delegates (ops, research)
	if _, err := e.create(main, tasks.CreateInput{RequestID: "d-ops"}); err != nil {
		t.Fatal(err)
	}

	// And the owner and supervisors still delegate in a project
	if _, err := e.create(e.own, tasks.CreateInput{RequestID: "d-own", ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
}
