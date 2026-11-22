package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// liveStanding is p1's standing supervisor, running, with an active attempt whose binding names
// kagent session ks, as [rig.liveTask] makes for an ordinary task.
func (r *rig) liveStanding(ks string) (taskID, sessionID string) {
	r.t.Helper()
	ctx := context.Background()
	v, err := r.svc.EnsureSupervisor(ctx, owner, "p1")
	if err != nil {
		r.t.Fatal(err)
	}
	sessionID = uuid.NewString()
	q := dbq.New(r.pool)
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{ID: sessionID, UserID: owner, Title: "standing"}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: sessionID, Kind: "claude", Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantCoordination,
	}); err != nil {
		r.t.Fatal(err)
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id = $2, kagent_agent_namespace = 'kagent', kagent_agent_name = 'agent' WHERE session_id = $1`, sessionID, ks)
	r.attempt(v.Task.Id, dbstate.AttemptSupervisor, dbstate.AttemptActive, sessionID)
	r.setStatus(v.Task.Id, "running", "")
	return v.Task.Id, sessionID
}

func (r *rig) state(id string) string {
	r.t.Helper()
	return r.taskStatus(id) + "/" + r.taskReason(id)
}

func TestStandingSupervisorRestsWhenNoTurnRuns(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")

	// Given a running standing supervisor whose session has no turn running and nothing on its way
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})

	// When the liveness sweep looks
	r.sweep(r.svc.LivenessSweep())

	// Then it is at rest, not stalled, and an update was published
	if got := r.state(id); got != "waiting/idle" {
		t.Fatalf("state = %s, want waiting/idle", got)
	}

	// And hours of rest change nothing
	r.setClock(time.Now().Add(10 * time.Hour))
	r.sweep(r.svc.LivenessSweep())
	if got := r.state(id); got != "waiting/idle" {
		t.Fatalf("state after a long rest = %s", got)
	}
}

func TestStandingSupervisorWakesWhenATurnStarts(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})
	r.sweep(r.svc.LivenessSweep())

	// Given a standing supervisor at rest whose attempt started long ago
	r.exec(`UPDATE task_attempts SET created_at = now() - interval '7 hours', reopened_at = NULL WHERE task_id = $1`, id)

	// When a message starts a turn in its session (a child's report, a loop, the owner)
	r.live.setTasks("ks-s", harnessTurn(time.Now(), 1, 0))
	r.sweep(r.svc.LivenessSweep())

	// Then it is running again, and its runtime bound starts from this wake
	if got := r.state(id); got != "running/" {
		t.Fatalf("state = %s, want running", got)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND reopened_at > now() - interval '1 minute'`, id); n != 1 {
		t.Fatal("the wake did not restart the runtime bound")
	}
	r.sweep(r.svc.LivenessSweep())
	if got := r.state(id); got != "running/" {
		t.Fatalf("state = %s: a seven-hour-old attempt woken now is inside its runtime bound", got)
	}

	// When the turn ends, Then it rests again
	r.live.setTasks("ks-s", nil)
	r.sweep(r.svc.LivenessSweep())
	if got := r.state(id); got != "waiting/idle" {
		t.Fatalf("state after the turn = %s", got)
	}
}

func TestStandingSupervisorStaysRunningWhileAMessageIsOnItsWayOrTheOwnerIsAsked(t *testing.T) {
	r := newRig(t)
	id, session := r.liveStanding("ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})

	// Given a message recorded for its session, not yet a turn in kagent
	r.exec(`INSERT INTO deliveries (id, idempotency_key, session_id, state) VALUES ($1, $1, $2, 'recorded')`,
		uuid.NewString(), session)
	r.sweep(r.svc.LivenessSweep())

	// Then it does not rest
	if got := r.state(id); got != "running/" {
		t.Fatalf("state with a message on its way = %s", got)
	}

	// Given the message ran and its turn now waits on the owner
	r.exec(`UPDATE deliveries SET state = 'completed' WHERE session_id = $1`, session)
	old := time.Now().Add(-time.Hour)
	r.live.setTasks("ks-s", []*a2a.Task{{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &old}}})
	r.sweep(r.svc.LivenessSweep())

	// Then it does not rest either
	if got := r.state(id); got != "running/" {
		t.Fatalf("state waiting on the owner = %s", got)
	}
}

func TestStandingSupervisorWedgedTurnStillStalls(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})

	// Given an open turn that records nothing new
	start := time.Now()
	r.live.setTasks("ks-s", stuckTurn(start.Add(-6*time.Hour), "m-1"))
	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())

	// When the stall timeout passes, Then it is stalled
	r.setClock(start.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.state(id); got != "blocked/stalled" {
		t.Fatalf("state = %s, want blocked/stalled", got)
	}
}

func TestStandingSupervisorAtRestWithAGoneSessionIsBlocked(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})
	r.sweep(r.svc.LivenessSweep())

	// Given a standing supervisor at rest whose kagent session is then gone
	r.live.set("ks-s", kagent.SessionLiveness{})
	r.sweep(r.svc.LivenessSweep())

	// Then it is blocked, session lost
	if got := r.state(id); got != "blocked/session_lost" {
		t.Fatalf("state = %s, want blocked/session_lost", got)
	}
}

func TestStandingSupervisorCrashRecoverableSessionRests(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")

	// Given a session kagent will revert on the next send
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true, Terminal: true, Recoverable: true})
	r.sweep(r.svc.LivenessSweep())

	// Then it is at rest, not lost
	if got := r.state(id); got != "waiting/idle" {
		t.Fatalf("state = %s, want waiting/idle", got)
	}
}

func TestAnyWakeRestartsTheRuntimeBound(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveStanding("ks-s")
	r.live.set("ks-s", kagent.SessionLiveness{Exists: true})
	r.sweep(r.svc.LivenessSweep())
	r.exec(`UPDATE task_attempts SET reopened_at = NULL WHERE task_id = $1`, id)

	// When anything moves it from rest to running (a follow-up, a report)
	r.exec(`UPDATE tasks SET status = 'running', reason = NULL WHERE id = $1`, id)

	// Then its current attempt's runtime bound starts again
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND reopened_at IS NOT NULL`, id); n != 1 {
		t.Fatal("no reopened_at after the wake")
	}

	// And an ordinary task's moves do not touch it
	other, _ := r.liveTask("ks-o")
	r.exec(`UPDATE task_attempts SET reopened_at = NULL WHERE task_id = $1`, other)
	r.exec(`UPDATE tasks SET status = 'waiting', reason = 'ci' WHERE id = $1`, other)
	r.exec(`UPDATE tasks SET status = 'running', reason = NULL WHERE id = $1`, other)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND reopened_at IS NOT NULL`, other); n != 0 {
		t.Fatal("an ordinary task's move restarted its runtime bound")
	}
}
