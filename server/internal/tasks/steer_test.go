package tasks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// mainPrincipal is the main thread acting on tasks.
func (r *rig) mainPrincipal() tasks.Principal {
	r.t.Helper()
	m, err := r.svc.Sessions.EnsureMain(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	p, err := r.svc.Resolve(context.Background(), tasks.Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: m.SessionID})
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// runningTurn gives the session a turn that kagent is running (delivered, with a task), which is
// the turn a steer can join.
func (r *rig) runningTurn(sessionID string) {
	r.t.Helper()
	r.drainTurns()
	r.openTurn(sessionID, uuid.NewString())
	r.exec(`UPDATE deliveries SET state = 'delivered', task_id = gen_random_uuid() WHERE session_id = $1 AND state = 'recorded'`, sessionID)
}

// drainTurns ends every open and queued turn.
func (r *rig) drainTurns() {
	r.t.Helper()
	for range 10 {
		r.finishTurns()
		if r.count(`SELECT count(*) FROM deliveries WHERE state IN ('queued', 'recorded', 'sending', 'delivered')`) == 0 {
			return
		}
	}
	r.t.Fatal("turns did not drain")
}

func steer(t *testing.T, r *rig, p tasks.Principal, task, req string) tasks.SteerResult {
	t.Helper()
	res, err := r.svc.Steer(context.Background(), p, tasks.SteerInput{TaskID: task, RequestID: req, Text: "also check the docs"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSteerJoinsTheRunningTurnOfASteerAgent(t *testing.T) {
	r := newRig(t)
	_, sup, child, _ := r.reportingChild()
	_, childSession := r.attemptOf(child)
	r.exec(`UPDATE agent_bindings SET kagent_agent_name = 'steer-test', kagent_agent_namespace = 'kagent' WHERE session_id = $1`, childSession)
	r.ses.Opts.SteerAgents = []string{"steer-test"}
	r.runningTurn(childSession)
	supP := r.agent(dbstate.RoleSupervisor, sup)

	res := steer(t, r, supP, child, "s1")
	if res.How != "steered" {
		t.Fatalf("how = %s, want steered", res.How)
	}
	if n := r.count(`SELECT count(*) FROM delivery_steers WHERE session_id = $1`, childSession); n != 1 {
		t.Fatalf("steers = %d", n)
	}
	// A retry finds the first send.
	if again := steer(t, r, supP, child, "s1"); again.MessageID != res.MessageID || again.How != "steered" {
		t.Fatalf("retry = %+v, first = %+v", again, res)
	}
	if n := r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1 AND content LIKE '[message from supervisor task %'`, childSession); n != 1 {
		t.Fatalf("messages = %d, want 1 after a retry", n)
	}
}

func TestSteerFallsBackToQueueing(t *testing.T) {
	r := newRig(t)
	_, _, child, _ := r.reportingChild()
	_, childSession := r.attemptOf(child)
	main := r.mainPrincipal()

	// An Agent without steers: the message waits behind the running turn.
	r.runningTurn(childSession)
	if res := steer(t, r, main, child, "q1"); res.How != "queued" {
		t.Fatalf("how = %s, want queued", res.How)
	}
	if n := r.count(`SELECT count(*) FROM delivery_steers WHERE session_id = $1`, childSession); n != 0 {
		t.Fatalf("steers = %d, want 0", n)
	}
	// No turn open: the message starts one.
	r.drainTurns()
	if res := steer(t, r, main, child, "q2"); res.How != "sent" {
		t.Fatalf("how = %s, want sent", res.How)
	}
}

func TestSteerIsLimitedToManagedLiveTasks(t *testing.T) {
	r := newRig(t)
	parent, sup, child, kid := r.reportingChild()
	supP := r.agent(dbstate.RoleSupervisor, sup)
	var e *tasks.Error
	// A supervisor cannot steer its own task, and a child steers nothing.
	if _, err := r.svc.Steer(context.Background(), supP, tasks.SteerInput{TaskID: parent, RequestID: "x", Text: "t"}); !errors.As(err, &e) || e.Reason != tasks.ReasonManagementDenied {
		t.Fatalf("own task: %v", err)
	}
	if _, err := r.svc.Steer(context.Background(), kid, tasks.SteerInput{TaskID: child, RequestID: "x", Text: "t"}); !errors.As(err, &e) || e.Reason != tasks.ReasonManagementDenied {
		t.Fatalf("child: %v", err)
	}
	r.exec(`UPDATE tasks SET status = 'cancelled', reason = NULL, version = version + 1 WHERE id = $1`, child)
	if _, err := r.svc.Steer(context.Background(), r.mainPrincipal(), tasks.SteerInput{TaskID: child, RequestID: "x", Text: "t"}); !errors.As(err, &e) || e.Reason != tasks.ReasonTaskTerminal {
		t.Fatalf("ended task: %v", err)
	}
}
