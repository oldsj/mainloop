package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// The owner lists a task's loops and pauses, fires, resumes and deletes one over REST; every
// response is held to the contract.
func TestRESTLoopControls(t *testing.T) {
	r := newRest(t)
	op := r.do("POST", "/tasks", map[string]any{"request_id": "c1", "title": "Coordinate", "brief": "b", "mode": "coordination"}, 202)
	id := op["task_id"].(string)
	// Armed by main, and not due for an hour: no runner looks at it during the test.
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO task_loops (id, owner_id, principal_key, request_id, recipient_key,
		subject_task_id, trigger, note, max_firings, min_interval_seconds, expires_at, next_attempt_at, baseline, last_fired_at)
		VALUES ('loop-1', 'local-dev-user', 'main:t', 'r1', 'main', $1, 'task', 'watch it', 20, 3600, now() + interval '1 day',
		        now() + interval '1 hour', 'queued', now())`, id); err != nil {
		t.Fatal(err)
	}

	// Main's time loop with no task, and a supervisor's loop: the main thread lists only main's two.
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO task_loops (id, owner_id, principal_key, request_id, recipient_key,
		trigger, note, max_firings, min_interval_seconds, expires_at, next_attempt_at, fire_at)
		VALUES ('loop-2', 'local-dev-user', 'main:t', 'r2', 'main', 'time', 'check in', 20, 60, now() + interval '1 day',
		        now() + interval '1 hour', now() + interval '1 hour'),
		       ('loop-3', 'local-dev-user', 'task:t', 'r3', 'parent-task:'||$1, 'time', 'mine', 20, 60, now() + interval '1 day',
		        now() + interval '1 hour', now() + interval '1 hour')`, id); err != nil {
		t.Fatal(err)
	}
	mainLoops := r.do("GET", "/main-thread/loops", nil, 200)["loops"].([]any)
	var mainIDs []string
	for _, l := range mainLoops {
		mainIDs = append(mainIDs, l.(map[string]any)["id"].(string))
	}
	if strings.Join(mainIDs, ",") != "loop-2,loop-1" {
		t.Fatalf("main's loops = %v", mainIDs)
	}
	if l := mainLoops[0].(map[string]any); l["task_id"] != nil || l["next_fire_at"] == nil || l["recipient"] != "main" {
		t.Fatalf("main's time loop = %v", l)
	}

	list := r.do("GET", "/tasks/"+id+"/loops", nil, 200)
	loops := list["loops"].([]any)
	if len(loops) != 2 {
		t.Fatalf("loops = %v", loops)
	}
	if l := loops[1].(map[string]any); l["state"] != "armed" || l["recipient"] != "main" || l["task_id"] != id ||
		len(l["recent_firings"].([]any)) != 0 {
		t.Fatalf("loop = %v", l)
	}
	if l := r.do("POST", "/loops/loop-1/pause", nil, 200); l["state"] != "paused" {
		t.Fatalf("paused = %v", l)
	}
	if c := r.do("POST", "/loops/loop-1/pause", nil, 409); reason(c) != tasks.ReasonLoopState {
		t.Fatalf("pause again = %v", c)
	}
	fired := r.do("POST", "/loops/loop-1/fire", map[string]any{"request_id": "f1"}, 200)
	firings := fired["recent_firings"].([]any)
	if fired["firings"] != 1.0 || len(firings) != 1 {
		t.Fatalf("fired = %v", fired)
	}
	f := firings[0].(map[string]any)
	if facts := f["facts"].([]any); f["delivery_state"] != "pending" || !strings.HasPrefix(facts[0].(string), "fired now by the owner") {
		t.Fatalf("firing = %v", f)
	}
	// Not held for the minimum interval since the loop's last firing an instant ago.
	var held bool
	if err := r.pool.QueryRow(context.Background(),
		`SELECT not_before > now() + interval '1 minute' FROM task_loop_firings WHERE loop_id = 'loop-1'`).Scan(&held); err != nil || held {
		t.Fatalf("held = %v, %v", held, err)
	}
	if again := r.do("POST", "/loops/loop-1/fire", map[string]any{"request_id": "f1"}, 200); again["firings"] != 1.0 {
		t.Fatalf("a retried fire fired again: %v", again)
	}
	if l := r.do("POST", "/loops/loop-1/resume", nil, 200); l["state"] != "armed" {
		t.Fatalf("resumed = %v", l)
	}
	if l := r.do("DELETE", "/loops/loop-1", nil, 200); l["state"] != "stopped" {
		t.Fatalf("deleted = %v", l)
	}
	if c := r.do("DELETE", "/loops/loop-1", nil, 409); reason(c) != tasks.ReasonLoopState {
		t.Fatalf("delete again = %v", c)
	}
	if c := r.do("POST", "/loops/loop-1/fire", map[string]any{"request_id": "f2"}, 409); reason(c) != tasks.ReasonLoopState {
		t.Fatalf("fire a stopped loop = %v", c)
	}
	if nf := r.do("POST", "/loops/missing/pause", nil, 404); reason(nf) != tasks.ReasonLoopNotFound {
		t.Fatalf("missing loop = %v", nf)
	}
	if nf := r.do("GET", "/tasks/missing/loops", nil, 404); reason(nf) != tasks.ReasonTaskNotFound {
		t.Fatalf("missing task = %v", nf)
	}
}

// A paused loop does not fire for a change, but hears of it once resumed; it still expires, and it
// still counts as a supervisor's wait. The task page shows a supervisor's loops on its own task
// and the loops watching a task on that task.
func TestPausedLoopHearsOnResumeAndStillExpires(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	parent, supSession, child, _ := r.reportingChild()
	owner := tasks.OwnerPrincipal(r.own.OwnerID)
	loop := r.arm(r.agentOf(supSession), taskLoop("l1", child, dbstate.TaskBlocked))
	other := r.arm(r.agentOf(supSession), tasks.LoopInput{RequestID: "t", Trigger: dbstate.LoopTriggerTime, AfterMinutes: 60, Note: "n"})
	for _, task := range []string{parent, child} {
		ls, err := r.svc.TaskLoops(ctx, owner, task)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{parent: 2, child: 1}[task]
		if len(ls) != want {
			t.Fatalf("loops of %s = %d, want %d", task, len(ls), want)
		}
	}
	if _, err := r.svc.PauseLoop(ctx, owner, loop); err != nil {
		t.Fatal(err)
	}
	r.setStatus(child, "blocked", "owner_decision")
	time.Sleep(150 * time.Millisecond)
	if n := r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop); n != 0 {
		t.Fatal("a paused loop fired")
	}
	if n := r.count(`SELECT count(*) FROM task_loops WHERE recipient_key = 'parent-task:' || $1 AND state IN ('armed', 'paused')`, parent); n != 2 {
		t.Fatalf("live loops = %d", n)
	}
	if _, err := r.svc.ResumeLoop(ctx, owner, loop); err != nil {
		t.Fatal(err)
	}
	r.eventually("fired on resume", func() bool {
		return r.count(`SELECT count(*) FROM task_loop_firings WHERE loop_id = $1`, loop) == 1
	})
	if _, err := r.svc.PauseLoop(ctx, owner, other); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE task_loops SET expires_at = now(), next_attempt_at = now() WHERE id = $1`, other)
	r.eventually("a paused loop expires", func() bool { return r.loopState(other) == "expired" })
}
