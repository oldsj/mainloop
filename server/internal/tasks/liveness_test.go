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
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// liveTask creates a running coordination task with one active attempt whose binding names
// kagentSessionID, current on the task, as the liveness sweep requires. The binding is a real
// agent_bindings row, resolved the same way KagentLiveness resolves one in production, so a test
// keys the fake kagent client by the kagent id, never Mainloop's own session/binding id. An empty
// kagentSessionID leaves the binding's kagent_session_id NULL, as a coordination attempt has
// before its brief is delivered.
func (r *rig) liveTask(kagentSessionID string) (taskID, bindingID string) {
	r.t.Helper()
	taskID = r.mustCreate(r.own, tasks.CreateInput{})
	bindingID = uuid.NewString()
	ctx := context.Background()
	q := dbq.New(r.pool)
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{ID: bindingID, UserID: owner, Title: "live"}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: bindingID, Kind: "claude", Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantCoordination,
	}); err != nil {
		r.t.Fatal(err)
	}
	if kagentSessionID != "" {
		r.exec(`UPDATE agent_bindings SET kagent_session_id = $2, kagent_agent_namespace = 'kagent', kagent_agent_name = 'agent' WHERE session_id = $1`,
			bindingID, kagentSessionID)
	}
	r.attempt(taskID, dbstate.AttemptSupervisor, dbstate.AttemptActive, bindingID)
	r.setStatus(taskID, "running", "")
	return taskID, bindingID
}

func TestLivenessBlocksATaskWhoseSessionIsGone(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{}) // Exists: false, kagent has no record of it
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "session_lost" {
		t.Fatalf("reason = %s, want session_lost", got)
	}
}

func TestLivenessBlocksATaskWhoseSessionIsTerminal(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true, Terminal: true})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "session_lost" {
		t.Fatalf("reason = %s, want session_lost", got)
	}
}

// Review P0: the sweep must resolve the binding's kagent_session_id and ask kagent about that, not
// about Mainloop's own binding id, which kagent never issued and would answer NotFound for.
func TestLivenessAsksKagentForTheResolvedSessionIdNotTheBindingId(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set(bindingID, kagent.SessionLiveness{}) // wrong key: answers gone if asked by binding id
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (the sweep must have asked for ks-1, not %s)", got, bindingID)
	}
}

// Review P0: a coordination attempt has no kagent session until its brief is delivered. That must
// not read as lost.
func TestLivenessSkipsAnAttemptWithNoKagentSessionYet(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("") // no kagent_session_id yet
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running", got)
	}
	if n := r.live.lookCount(); n != 0 {
		t.Fatalf("asked kagent about a session that does not exist yet: calls=%d", n)
	}
}

func TestLivenessBlocksAStalledTask(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now().Add(-46*time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %s, want stalled", got)
	}
}

// Review P1: kagent's own Session.updated_at does not move on a turn, so activity comes from the
// A2A tasks of the session's contexts instead. A working task counts as activity right now,
// however old its own last-reported status change, so a 50-minute turn does not read as stalled.
func TestLivenessActivityResetsTheStallClock(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now().Add(-46*time.Minute)) // would stall on its own
	old := time.Now().Add(-50 * time.Minute)
	r.live.setTasks("ks-1", []*a2a.Task{{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &old}}})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (a working task is activity now)", got)
	}
}

func TestLivenessBlocksATaskPastItsRuntimeBound(t *testing.T) {
	r := newRig(t)
	id, bindingID := r.liveTask("ks-1")
	r.exec(`UPDATE task_attempts SET created_at = now() - interval '7 hours' WHERE task_id = $1`, id)
	// The session itself looks healthy and recently active: only the overall bound catches this one.
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	r.activity.set(bindingID, time.Now())
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "runtime_limit" {
		t.Fatalf("reason = %s, want runtime_limit", got)
	}
}

// Review P2: the observation is a slow, uncached network call made with no lock held. If a report
// reaches the task first — completing it, or blocking it on an owner question — that report owns
// the task, and the liveness signal that raced it must be dropped, not overwrite it.
func TestLivenessRaceWithACompletingReportLeavesTheTaskAlone(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{}) // would otherwise block it session_lost
	r.live.onObserve = func() {
		r.exec(`UPDATE tasks SET status = 'waiting', reason = 'publication', version = version + 1 WHERE id = $1`, id)
	}
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "waiting" {
		t.Fatalf("status = %s, want waiting (the report that landed during the observation owns the task)", got)
	}
	if got := r.taskReason(id); got != "publication" {
		t.Fatalf("reason = %s, want publication", got)
	}
}

// Once a task is blocked, the sweep's own query stops selecting it: looking again costs nothing and
// changes nothing.
func TestLivenessSweepLeavesABlockedTaskAlone(t *testing.T) {
	r := newRig(t)
	r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{})
	r.sweep(r.svc.LivenessSweep())
	n := r.live.lookCount()
	r.sweep(r.svc.LivenessSweep())
	if r.live.lookCount() != n {
		t.Fatal("the sweep looked at an already-blocked task's session again")
	}
}
