package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// runningStanding starts p1's standing supervisor through main's supervisor_send, with the
// reconciler running, and answers its task, session and principal.
func (r *rig) runningStanding() (id, session string, sup tasks.Principal) {
	r.t.Helper()
	if _, err := r.svc.SupervisorSend(context.Background(), r.mainPrincipal(), tasks.SupervisorSendInput{
		ProjectID: "p1", RequestID: "start", Text: "first batch",
	}); err != nil {
		r.t.Fatal(err)
	}
	id = r.str(`SELECT id FROM tasks WHERE project_id = 'p1' AND standing`)
	_, session = r.attemptOf(id)
	return id, session, r.agent(dbstate.RoleSupervisor, session)
}

func (r *rig) restart(req string, fresh bool) {
	r.t.Helper()
	if _, err := r.svc.RestartSupervisor(context.Background(), owner, "p1", req, fresh); err != nil {
		r.t.Fatal(err)
	}
}

func TestStandingRestartKeepsItsWorkersAndTask(t *testing.T) {
	r := newRig(t)
	r.run()
	id, oldSession, sup := r.runningStanding()
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "status", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}

	// Given a standing supervisor with a running worker
	child := r.mustCreate(sup, tasks.CreateInput{Title: "Read the flaky test", Brief: "look into it"})
	r.eventually("worker running", func() bool { return r.taskStatus(child) == "running" })
	_, childSession := r.attemptOf(child)
	kid := r.agent(dbstate.RoleChild, childSession)

	// When the owner restarts it
	r.restart("rs-1", false)

	// Then the worker keeps its authority while the supervisor's attempt is replaced, and reports to it
	rep := report(kid, "found it", dbstate.OutcomeBlocked)
	rep.Summary = "found it: needs a decision"
	if _, err := r.svc.Report(context.Background(), kid, rep); err != nil {
		t.Fatalf("worker report during the restart: %v", err)
	}

	// And the supervisor runs again on the same task, in a new session, with the worker kept
	r.handedOff(id)
	_, newSession := r.attemptOf(id)
	if newSession == oldSession {
		t.Fatal("the restart kept the old session")
	}
	if got := r.taskStatus(child); got == "cancelled" {
		t.Fatal("the restart cancelled the worker")
	}

	// And its new brief says what came before: the last report and its open worker
	brief := r.firstMessage(newSession)
	for _, want := range []string{"## Continuation", "You were restarted", "Your last report (progress)", "/tasks/" + child, "Read the flaky test", "Standing supervisor"} {
		if !strings.Contains(brief, want) {
			t.Errorf("successor brief lacks %q:\n%s", want, brief)
		}
	}

	// And the worker's report reaches the new session, not the old one
	r.eventually("worker report delivered to the successor", func() bool {
		r.finishTurns()
		return r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '%found it%'`, newSession) == 1
	})

	// And a retry of the same request finds the same restart
	r.restart("rs-1", false)
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind = 'retry'`, id); n != 1 {
		t.Fatalf("restarts = %d", n)
	}
}

func TestStandingResetLeavesOutTheOldAccount(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _, sup := r.runningStanding()
	if _, err := r.svc.Report(context.Background(), sup, report(sup, "status", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}

	// When the owner resets it
	r.restart("reset-1", true)
	r.handedOff(id)

	// Then its new brief has the facts and not the old session's report
	_, session := r.attemptOf(id)
	brief := r.firstMessage(session)
	if !strings.Contains(brief, "This was a reset") || strings.Contains(brief, "Your last report") {
		t.Fatalf("reset brief:\n%s", brief)
	}
}

func TestMainRestartsAStandingSupervisorThreeTimesAnHour(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _, _ := r.runningStanding()
	main := r.mainPrincipal()

	// Given main restarts it three times
	for i := range 3 {
		v := r.view(r.own, id)
		if _, err := r.svc.Retry(context.Background(), main, action(id, v, "m-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		r.handedOff(id)
	}

	// When main restarts it a fourth time within the hour, Then it is refused
	v := r.view(r.own, id)
	_, err := r.svc.Retry(context.Background(), main, action(id, v, "m-d"))
	wantRefusal(t, err, 409, tasks.ReasonHandoffLimit)

	// And the owner still can
	r.restart("own-1", false)
	r.handedOff(id)
}

func TestRestartNeedsAStandingSupervisor(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.RestartSupervisor(context.Background(), owner, "p1", "rs-1", false)
	wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)
	_, err = e.svc.RestartSupervisor(context.Background(), owner, "foreign", "rs-2", false)
	wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
}
