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

// kagent keeps a turn "working" until it ends, and a turn whose native process wedged never ends,
// so a working task is not proof of life. Prod incident: a Claude turn hung inside a tool call at
// 14:58, kagent reported the task working with a 14:48 status time for five hours, and the sweep
// read that as activity every minute. What counts is kagent recording something new for the turn.
//
// The sweep has no clock of its own beyond Service.Now, so these tests move it: the first look at a
// working turn starts its stall clock, and later looks find it changed or not.
func stuckTurn(old time.Time, newest string) []*a2a.Task {
	return []*a2a.Task{{
		ID:      "t-1",
		Status:  a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &old},
		History: []*a2a.Message{{ID: newest}},
	}}
}

func TestLivenessBlocksAWorkingTurnThatRecordsNothingNew(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	old := start.Add(-6 * time.Hour)
	r.live.setTasks("ks-1", stuckTurn(old, "m-1"))

	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (the first look only starts the clock)", got)
	}

	r.setClock(start.Add(40 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running (40 minutes is inside the stall timeout)", got)
	}

	r.setClock(start.Add(46 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked (a turn kagent calls working but that recorded nothing for 46 minutes)", got)
	}
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %s, want stalled", got)
	}
}

func TestLivenessKeepsAWorkingTurnThatKeepsRecordingProgress(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now()
	old := start.Add(-6 * time.Hour)

	// Every half hour a new message lands: three hours of a busy turn.
	for i := range 7 {
		r.live.setTasks("ks-1", stuckTurn(old, "m-"+string(rune('a'+i))))
		r.setClock(start.Add(time.Duration(i) * 30 * time.Minute))
		r.sweep(r.svc.LivenessSweep())
		if got := r.taskStatus(id); got != "running" {
			t.Fatalf("step %d: status = %s, want running (the turn is still recording progress)", i, got)
		}
	}
}

func TestLivenessRecordsTheProgressItSaw(t *testing.T) {
	r := newRig(t)
	id, _ := r.liveTask("ks-1")
	r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
	start := time.Now().Truncate(time.Microsecond)
	r.live.setTasks("ks-1", stuckTurn(start.Add(-time.Hour), "m-1"))
	r.setClock(start)
	r.sweep(r.svc.LivenessSweep())

	var marker *string
	var at *time.Time
	if err := r.pool.QueryRow(context.Background(),
		`SELECT progress_marker, progress_at FROM task_attempts WHERE task_id = $1`, id).Scan(&marker, &at); err != nil {
		t.Fatal(err)
	}
	if marker == nil || *marker == "" || at == nil || !at.Equal(start) {
		t.Fatalf("progress = %v at %v, want a marker recorded at %v", marker, at, start)
	}

	// Seeing the same thing again must not move the time it was first seen.
	r.setClock(start.Add(10 * time.Minute))
	r.sweep(r.svc.LivenessSweep())
	if err := r.pool.QueryRow(context.Background(),
		`SELECT progress_marker, progress_at FROM task_attempts WHERE task_id = $1`, id).Scan(&marker, &at); err != nil {
		t.Fatal(err)
	}
	if at == nil || !at.Equal(start) {
		t.Fatalf("progress_at = %v, want it left at %v", at, start)
	}
}

// P2: an agent waiting on the owner (input-required or auth-required) is not stalled either — it
// is blocked on a reply, not idle — though the runtime bound still applies regardless of activity.
func TestLivenessWaitingOnTheOwnerResetsTheStallClock(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			r := newRig(t)
			id, bindingID := r.liveTask("ks-1")
			r.live.set("ks-1", kagent.SessionLiveness{Exists: true})
			r.activity.set(bindingID, time.Now().Add(-46*time.Minute)) // would stall on its own
			old := time.Now().Add(-50 * time.Minute)
			r.live.setTasks("ks-1", []*a2a.Task{{ID: "t-1", Status: a2a.TaskStatus{State: state, Timestamp: &old}}})
			r.sweep(r.svc.LivenessSweep())
			if got := r.taskStatus(id); got != "running" {
				t.Fatalf("status = %s, want running (waiting on the owner is activity now)", got)
			}
		})
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

// setClock moves the sweep's clock and the clock KagentLiveness stamps "now" with together, as one
// process clock does in production.
func (r *rig) setClock(at time.Time) {
	now := func() time.Time { return at }
	r.svc.Now = now
	r.svc.Liveness.(*tasks.KagentLiveness).Now = now
}
