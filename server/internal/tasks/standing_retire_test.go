package tasks_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestOwnerRetiresAStandingSupervisorWithItsWorkers(t *testing.T) {
	r := newRig(t)
	r.run()
	ctx := context.Background()
	id, _, sup := r.runningStanding()

	// Given a standing supervisor with a running worker
	child := r.mustCreate(sup, tasks.CreateInput{Title: "Read the flaky test", Brief: "look into it"})
	r.eventually("worker running", func() bool { return r.taskStatus(child) == "running" })

	// When the owner retires it
	op, err := r.svc.RetireSupervisor(ctx, owner, "p1", "rt-1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != "cancel" {
		t.Fatalf("retire operation kind = %s", op.Kind)
	}

	// Then its worker is cancelled at once
	if got := r.taskStatus(child); got != "cancelled" {
		t.Fatalf("worker = %s, want cancelled", got)
	}

	// And the supervisor ends cancelled, with nothing left running
	r.eventually("supervisor retired with nothing running", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "cancelled" &&
			r.count(`SELECT count(*) FROM task_attempts WHERE task_id IN ($1, $2) AND state IN ('creating', 'active', 'draining', 'fenced')`, id, child) == 0 &&
			r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND state NOT IN ('completed', 'blocked')`, id) == 0
	})

	// And a retry of the same request finds the same operation
	again, err := r.svc.RetireSupervisor(ctx, owner, "p1", "rt-1")
	if err != nil || again.Id != op.Id {
		t.Fatalf("retry = %v, %v; want operation %s", again.Id, err, op.Id)
	}

	// And the project has no supervisor: a restart or another retire finds none
	_, err = r.svc.RestartSupervisor(ctx, owner, "p1", "rs-after", false)
	wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)
	_, err = r.svc.RetireSupervisor(ctx, owner, "p1", "rt-2")
	wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)

	// When the project is used again, Then a new standing supervisor starts, and the retired one
	// stays as its history
	v, err := r.svc.EnsureSupervisor(ctx, owner, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Task.Id == id || !v.Task.Standing {
		t.Fatalf("next supervisor = %+v", v.Task)
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND standing`); n != 2 {
		t.Fatalf("standing tasks = %d, want the retired one and the new one", n)
	}
	if n := r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind = 'create' AND request_id = 'standing-supervisor:p1:2'`, v.Task.Id); n != 1 {
		t.Fatal("the new supervisor was not made by its own create request")
	}
	if again, err := r.svc.EnsureSupervisor(ctx, owner, "p1"); err != nil || again.Task.Id != v.Task.Id {
		t.Fatalf("ensure again = %v, %v", again.Task.Id, err)
	}
}

func TestRetireNeedsTheOwnersStandingSupervisor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Given no standing supervisor, or a project that is not the owner's, Then there is nothing to retire
	_, err := e.svc.RetireSupervisor(ctx, owner, "p1", "rt-1")
	wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)
	_, err = e.svc.RetireSupervisor(ctx, owner, "foreign", "rt-2")
	wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)

	// And a request id is required
	id, _ := e.standing("sup-s")
	_, err = e.svc.RetireSupervisor(ctx, owner, "p1", "")
	wantRefusal(t, err, 422, tasks.ReasonInvalidRequest)

	// And a second standing supervisor that has not ended is still refused by the database
	if _, err := e.pool.Exec(ctx, `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, standing)
		VALUES ('dup', 'owner-1', 'p1', 'dup', 't', 'b', 'coordination', 'claude', 'default', true)`); err == nil {
		t.Fatalf("a second open standing supervisor beside %s was accepted", id)
	}
}
