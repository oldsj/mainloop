package tasks_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// bindKagent names the kagent session behind a Mainloop session, as delivery does once it has made
// one, so the liveness adapter asks the fake about it.
func (r *rig) bindKagent(session, ks string) {
	r.t.Helper()
	r.exec(`UPDATE agent_bindings SET kagent_session_id = $2, kagent_agent_namespace = 'kagent', kagent_agent_name = 'agent' WHERE session_id = $1`, session, ks)
}

func TestGoneStandingSessionIsRecoveredWithItsWorkers(t *testing.T) {
	r := newRig(t)
	r.run()
	id, session, sup := r.runningStanding()
	child := r.mustCreate(sup, tasks.CreateInput{Title: "Read the flaky test", Brief: "look into it"})
	r.eventually("worker running", func() bool { return r.taskStatus(child) == "running" })
	attempt, _ := r.attemptOf(id)

	// Given its kagent session is gone
	r.bindKagent(session, "ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{})

	// When the liveness sweep looks
	r.sweep(r.svc.LivenessSweep())

	// Then it runs again on the same task, in a new session, and its worker was kept
	r.handedOff(id)
	_, newSession := r.attemptOf(id)
	if newSession == session {
		t.Fatal("recovery kept the gone session")
	}
	if got := r.taskStatus(child); got == "cancelled" {
		t.Fatal("recovery cancelled the worker")
	}
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND request_id = $2`, id, "standing-recover:"+attempt); n != 1 {
		t.Fatalf("recoveries = %d", n)
	}
}

func TestASendThatFindsTheSessionGoneRecoversItAndIsSentAfter(t *testing.T) {
	r := newRig(t)
	r.run()
	id, session, _ := r.runningStanding()
	r.finishTurns()
	r.bindKagent(session, "ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{})
	main := r.mainPrincipal()

	// When main sends work to a supervisor whose session is gone
	_, err := r.svc.SupervisorSend(context.Background(), main, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-2", Text: "second batch"})

	// Then it is told the supervisor is starting, and a recovery is under way
	wantRefusal(t, err, 409, tasks.ReasonSupervisorStarting)

	// And the sweep noticing too does not start a second one
	r.sweep(r.svc.LivenessSweep())
	r.handedOff(id)
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind = 'retry'`, id); n != 1 {
		t.Fatalf("recoveries = %d", n)
	}

	// When main sends again under the same request id, Then it reaches the new session
	res, err := r.svc.SupervisorSend(context.Background(), main, tasks.SupervisorSendInput{ProjectID: "p1", RequestID: "s-2", Text: "second batch"})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID == session {
		t.Fatal("sent to the gone session")
	}
}

func TestAStandingSupervisorThatKeepsLosingItsSessionIsBlockedForTheOwner(t *testing.T) {
	r := newRig(t)
	r.run()
	id, _, _ := r.runningStanding()

	// Given it was restarted three times this hour
	for _, req := range []string{"a", "b", "c"} {
		r.restart(req, false)
		r.handedOff(id)
	}

	// When its session is gone again
	_, session := r.attemptOf(id)
	r.bindKagent(session, "ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{})
	r.sweep(r.svc.LivenessSweep())

	// Then it is not restarted a fourth time: it is blocked, session lost, for the owner
	if got := r.state(id); got != "blocked/session_lost" {
		t.Fatalf("state = %s, want blocked/session_lost", got)
	}
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind = 'retry'`, id); n != 3 {
		t.Fatalf("restarts = %d", n)
	}

	// And the owner can still restart it
	r.restart("own", false)
	r.handedOff(id)
}
