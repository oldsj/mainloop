package tasks_test

import (
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// liveTask creates a running code task with one active attempt bound to sessionID, current on the
// task, as the liveness sweep requires.
func (r *rig) liveTask(sessionID string) string {
	r.t.Helper()
	id := r.codeTask()
	r.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, sessionID)
	r.setStatus(id, "running", "")
	return id
}

func TestLivenessBlocksATaskWhoseSessionIsGone(t *testing.T) {
	r := newRig(t)
	id := r.liveTask("sess-1")
	r.live.set("sess-1", kagent.SessionLiveness{}) // Exists: false, kagent has no record of it
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
	id := r.liveTask("sess-1")
	r.live.set("sess-1", kagent.SessionLiveness{Exists: true, Terminal: true, LastActivity: time.Now()})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "session_lost" {
		t.Fatalf("reason = %s, want session_lost", got)
	}
}

func TestLivenessBlocksAStalledTask(t *testing.T) {
	r := newRig(t)
	id := r.liveTask("sess-1")
	r.svc.Limits.StallTimeout = 45 * time.Minute
	r.live.set("sess-1", kagent.SessionLiveness{Exists: true, LastActivity: time.Now().Add(-46 * time.Minute)})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "stalled" {
		t.Fatalf("reason = %s, want stalled", got)
	}
}

// Fresh activity, even well past when a cold session would have been stalled, resets the clock: the
// task stays running.
func TestLivenessActivityResetsTheStallClock(t *testing.T) {
	r := newRig(t)
	id := r.liveTask("sess-1")
	r.svc.Limits.StallTimeout = 45 * time.Minute
	r.live.set("sess-1", kagent.SessionLiveness{Exists: true, LastActivity: time.Now()})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s, want running", got)
	}
}

func TestLivenessBlocksATaskPastItsRuntimeBound(t *testing.T) {
	r := newRig(t)
	id := r.liveTask("sess-1")
	r.svc.Limits.MaxRuntime = 6 * time.Hour
	r.exec(`UPDATE task_attempts SET created_at = now() - interval '7 hours' WHERE task_id = $1`, id)
	// The session itself looks healthy and recently active: only the overall bound catches this one.
	r.live.set("sess-1", kagent.SessionLiveness{Exists: true, LastActivity: time.Now()})
	r.sweep(r.svc.LivenessSweep())
	if got := r.taskStatus(id); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if got := r.taskReason(id); got != "runtime_limit" {
		t.Fatalf("reason = %s, want runtime_limit", got)
	}
}

// Once a task is blocked, the sweep's own query stops selecting it: looking again costs nothing and
// changes nothing.
func TestLivenessSweepLeavesABlockedTaskAlone(t *testing.T) {
	r := newRig(t)
	r.liveTask("sess-1")
	r.live.set("sess-1", kagent.SessionLiveness{})
	r.sweep(r.svc.LivenessSweep())
	n := r.live.lookCount()
	r.sweep(r.svc.LivenessSweep())
	if r.live.lookCount() != n {
		t.Fatal("the sweep looked at an already-blocked task's session again")
	}
}
