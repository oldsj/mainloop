package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func (e *env) hold(p tasks.Principal, id string, d time.Duration, reason string) error {
	e.t.Helper()
	in := tasks.HoldInput{TaskID: id, Reason: reason}
	if d != 0 {
		in.Until = time.Now().Add(d)
	}
	_, err := e.svc.Hold(context.Background(), p, in)
	return err
}

func (e *env) isHeld(id string) bool {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM tasks WHERE id = $1 AND held_until IS NOT NULL`, id) == 1
}

func (e *env) openAttention(taskID string) {
	e.t.Helper()
	e.exec(`INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, task_id, title, needed, href)
		VALUES (gen_random_uuid()::text, $1, 'owner_decision', 'test', gen_random_uuid()::text, $2, 't', 'n', '/')`, owner, taskID)
}

func TestHoldIsMainOnlyBoundedAndShown(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	worker := tasks.Principal{OwnerID: owner, Role: dbstate.RoleSupervisor, BindingID: "b"}
	wantRefusal(t, e.hold(worker, id, time.Hour, ""), 403, tasks.ReasonManagementDenied)
	wantRefusal(t, e.hold(e.own, id, tasks.MaxHold+time.Hour, ""), 422, tasks.ReasonInvalidRequest)
	wantRefusal(t, e.hold(e.own, id, -time.Minute, ""), 422, tasks.ReasonInvalidRequest)
	wantRefusal(t, e.hold(e.own, "nope", time.Hour, ""), 404, tasks.ReasonTaskNotFound)

	if err := e.hold(e.own, id, time.Hour, "waiting on review"); err != nil {
		t.Fatal(err)
	}
	v := e.view(e.own, id)
	if v.Task.HeldUntil == nil || v.Task.HeldReason == nil || *v.Task.HeldReason != "waiting on review" {
		t.Fatalf("view does not show the hold: %+v", v.Task)
	}
	if err := e.hold(e.own, id, 0, ""); err != nil {
		t.Fatal(err)
	}
	if e.isHeld(id) {
		t.Fatal("clearing left the hold")
	}
}

// A hold never hides a failure: it ends the moment the task stops being live or anything needs
// the owner about it, and such a task cannot be held.
func TestHoldEndsWhenTheTaskNeedsTheOwner(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	if err := e.hold(e.own, id, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	e.openAttention(id)
	if e.isHeld(id) {
		t.Fatal("an open attention item left the hold in place")
	}
	wantRefusal(t, e.hold(e.own, id, time.Hour, ""), 409, tasks.ReasonHoldNeedsOwner)

	other := e.mustCreate(e.own, tasks.CreateInput{})
	if err := e.hold(e.own, other, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE tasks SET status = 'failed' WHERE id = $1`, other)
	if e.isHeld(other) {
		t.Fatal("a failed task kept its hold")
	}
	wantRefusal(t, e.hold(e.own, other, time.Hour, ""), 409, tasks.ReasonHoldNotLive)
}

// When a hold runs out the sweep tells main once and clears it.
func TestExpiredHoldNudgesMainOnce(t *testing.T) {
	r := newRig(t)
	id := r.mustCreate(r.own, tasks.CreateInput{Title: "Watch the PR"})
	if err := r.hold(r.own, id, time.Hour, "until review"); err != nil {
		t.Fatal(err)
	}
	r.sweep(r.svc.HoldSweep())
	if !r.isHeld(id) {
		t.Fatal("a hold that has not run out was cleared")
	}
	r.exec(`UPDATE tasks SET held_until = now() - interval '1 minute' WHERE id = $1`, id)
	r.sweep(r.svc.HoldSweep())
	if r.isHeld(id) {
		t.Fatal("the expired hold was not cleared")
	}
	nudges := `SELECT count(*) FROM session_messages m JOIN agent_bindings b ON b.session_id = m.session_id
		WHERE b.role = 'main' AND m.content LIKE '[mainloop] The hold on task ' || $1 || '%until review%'`
	if n := r.count(nudges, id); n != 1 {
		t.Fatalf("main got %d nudges, want 1", n)
	}
	r.sweep(r.svc.HoldSweep())
	if n := r.count(nudges, id); n != 1 {
		t.Fatalf("a second sweep sent again: %d nudges", n)
	}
}
