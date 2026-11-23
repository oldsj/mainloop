package tasks_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// rootIn is a running root supervisor task in project (or none), and its principal.
func (r *rig) rootIn(project, binding string) tasks.Principal {
	r.t.Helper()
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: project})
	r.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, binding)
	r.setStatus(root, "running", "")
	return r.agent(dbstate.RoleSupervisor, binding)
}

func intp(n int) *int { return &n }

func TestAProjectRefusesWorkersOverItsLimit(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenPerParent, r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxWorkersPerProject = 10, 10, 3

	// Given two supervisors in p1 and one with no project
	a, b, loose := r.rootIn("p1", "sup-a"), r.rootIn("p1", "sup-b"), r.rootIn("", "sup-c")

	// When they delegate past p1's default limit of 3
	r.mustCreate(a, tasks.CreateInput{})
	r.mustCreate(a, tasks.CreateInput{})
	r.mustCreate(b, tasks.CreateInput{})
	_, err := r.create(b, tasks.CreateInput{})

	// Then the fourth p1 worker is refused, whichever supervisor asks
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)
	_, err = r.create(a, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)
	// And work outside the project is not counted against it
	r.mustCreate(loose, tasks.CreateInput{})
	if n := r.count(`SELECT total FROM task_capacity_refusals WHERE reason = 'project_capacity'`); n != 2 {
		t.Fatalf("project_capacity refusals counted = %d", n)
	}

	// When the owner raises p1's limit to 4, Then one more is admitted, and no more
	if _, err := r.svc.SetWorkerLimit(context.Background(), owner, "p1", intp(4), tasks.LimitChanger{}); err != nil {
		t.Fatal(err)
	}
	r.mustCreate(b, tasks.CreateInput{})
	_, err = r.create(b, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)

	// When the owner lowers it below the running count, Then nothing running is touched
	w, err := r.svc.SetWorkerLimit(context.Background(), owner, "p1", intp(1), tasks.LimitChanger{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Running != 4 || w.Effective != 1 || r.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND parent_task_id IS NOT NULL AND status = 'cancelled'`) != 0 {
		t.Fatalf("after lowering: %+v", w)
	}
}

func TestAStandingSupervisorIsBoundByItsProjectNotThePerParentLimit(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenPerParent, r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxWorkersPerProject = 2, 10, 3
	r.run()
	_, _, sup := r.runningStanding()

	// When the standing supervisor delegates more than the per-parent limit of 2
	for range 3 {
		r.mustCreate(sup, tasks.CreateInput{Brief: "look into it"})
	}
	_, err := r.create(sup, tasks.CreateInput{Brief: "one more"})

	// Then only its project's limit of 3 refuses it
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)

	// And an ordinary supervisor in the same project is still bound by the per-parent limit, and
	// counts against the project: the project is full
	other := r.rootIn("p1", "sup-o")
	_, err = r.create(other, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonProjectCapacity)
}

func TestTheGlobalLimitStillApplies(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenPerParent, r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxWorkersPerProject = 10, 2, 5
	a := r.rootIn("p1", "sup-a")
	r.mustCreate(a, tasks.CreateInput{})
	r.mustCreate(a, tasks.CreateInput{})

	// When the project has room but the owner has none
	_, err := r.create(a, tasks.CreateInput{})

	// Then it is the global limit that refuses
	wantRefusal(t, err, 409, tasks.ReasonGlobalCapacity)
	if w, err := r.svc.WorkerLimit(context.Background(), owner, "p1"); err != nil || w.Global != 2 || w.Effective != 5 || w.Running != 2 {
		t.Fatalf("view = %+v, %v", w, err)
	}
}

func TestWorkerLimitChangesAreRecorded(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxWorkersPerProject = 6
	ctx := context.Background()

	// Given a project with no limit of its own
	w, err := r.svc.WorkerLimit(ctx, owner, "p1")
	if err != nil || w.Max != nil || w.Default != 6 || w.Effective != 6 || w.Running != 0 {
		t.Fatalf("unset = %+v, %v", w, err)
	}

	// When the owner sets 8, then main sets 8 again and then 2, then main resets it
	steps := []struct {
		max *int
		by  tasks.LimitChanger
	}{{intp(8), tasks.LimitChanger{}}, {intp(8), tasks.LimitChanger{SessionID: "main-1"}}, {intp(2), tasks.LimitChanger{SessionID: "main-1"}}, {nil, tasks.LimitChanger{SessionID: "main-1"}}}
	for _, s := range steps {
		if w, err = r.svc.SetWorkerLimit(ctx, owner, "p1", s.max, s.by); err != nil {
			t.Fatal(err)
		}
	}

	// Then it is back on the default, and every change (not the repeat) is recorded with who made it
	if w.Max != nil || w.Effective != 6 {
		t.Fatalf("after reset = %+v", w)
	}
	got := r.str(`SELECT string_agg(changed_by || ':' || coalesce(session_id, '-') || ':' || coalesce(old_max_workers::text, 'default') || '>' || coalesce(new_max_workers::text, 'default'), ',' ORDER BY id)
		FROM project_worker_limit_audit WHERE project_id = 'p1'`)
	if want := "owner:-:default>8,main:main-1:8>2,main:main-1:2>default"; got != want {
		t.Fatalf("audit = %s, want %s", got, want)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE project_worker_limit_audit SET new_max_workers = 99`); err == nil {
		t.Fatal("an audit row was rewritten")
	}

	// And a limit out of range, or another owner's or a missing project, is refused
	for _, v := range []int{0, tasks.MaxWorkerLimit + 1} {
		_, err := r.svc.SetWorkerLimit(ctx, owner, "p1", intp(v), tasks.LimitChanger{})
		wantRefusal(t, err, 422, tasks.ReasonInvalidRequest)
	}
	for _, p := range []string{"foreign", "missing"} {
		_, err := r.svc.SetWorkerLimit(ctx, owner, p, intp(3), tasks.LimitChanger{})
		wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
		_, err = r.svc.WorkerLimit(ctx, owner, p)
		wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
	}
}

func TestTheOwnerSetsTheWorkerLimitOverREST(t *testing.T) {
	r := newRest(t)

	// Given a project on the default limit
	got := r.do("GET", "/projects/p1/worker-limit", nil, 200)
	if got["max_workers"] != nil || got["default_max_workers"] != float64(6) || got["effective_max_workers"] != float64(6) || got["workers_running"] != float64(0) {
		t.Fatalf("unset = %v", got)
	}

	// When the owner sets 9, Then the view and the project both carry it
	got = r.do("PUT", "/projects/p1/worker-limit", map[string]any{"max_workers": 9}, 200)
	if got["max_workers"] != float64(9) || got["effective_max_workers"] != float64(9) {
		t.Fatalf("set = %v", got)
	}
	if p := r.do("GET", "/projects/p1", nil, 200); p["max_workers"] != float64(9) {
		t.Fatalf("project = %v", p)
	}

	// When the owner sends null, Then it is back on the default
	if got = r.do("PUT", "/projects/p1/worker-limit", map[string]any{"max_workers": nil}, 200); got["max_workers"] != nil || got["effective_max_workers"] != float64(6) {
		t.Fatalf("reset = %v", got)
	}

	// And a value out of range or an unknown project is refused
	r.do("PUT", "/projects/p1/worker-limit", map[string]any{"max_workers": 0}, 422)
	r.do("PUT", "/projects/p1/worker-limit", map[string]any{"max_workers": 101}, 422)
	r.do("GET", "/projects/nope/worker-limit", nil, 404)
}
