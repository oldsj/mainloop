package tasks_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// standing starts p1's standing supervisor and gives it a live attempt bound to binding.
func (e *env) standing(binding string) (string, tasks.Principal) {
	e.t.Helper()
	v, err := e.svc.EnsureSupervisor(context.Background(), owner, "p1")
	if err != nil {
		e.t.Fatal(err)
	}
	e.attempt(v.Task.Id, dbstate.AttemptSupervisor, dbstate.AttemptActive, binding)
	e.setStatus(v.Task.Id, "running", "")
	return v.Task.Id, e.agent(dbstate.RoleSupervisor, binding)
}

func TestEnsureSupervisorCreatesOnePerProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Given a project with no standing supervisor
	// When the owner and main start it at once, several times
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.svc.EnsureSupervisor(ctx, owner, "p1")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = v.Task.Id
		}()
	}
	wg.Wait()

	// Then there is one standing supervisor, a root coordination task of the project, queued for its
	// first attempt by one create operation
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ensure answered two tasks: %v", ids)
		}
	}
	if n := e.count(`SELECT count(*) FROM tasks WHERE project_id = 'p1' AND standing`); n != 1 {
		t.Fatalf("%d standing tasks for p1", n)
	}
	if n := e.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND kind = 'create'`, ids[0]); n != 1 {
		t.Fatalf("%d create operations", n)
	}
	v := e.view(e.own, ids[0])
	task := v.Task
	if !task.Standing || task.Mode != "coordination" || task.ParentTaskId != nil || task.ProjectId == nil ||
		*task.ProjectId != "p1" || task.Title != "Supervisor of o/p1" || task.CreatorBindingId != nil {
		t.Fatalf("task = %+v", task)
	}

	// And another project gets its own
	other, err := e.svc.EnsureSupervisor(ctx, owner, "p2")
	if err != nil || other.Task.Id == ids[0] {
		t.Fatalf("p2 supervisor = %v, %v", other.Task.Id, err)
	}
}

func TestEnsureSupervisorRefusesForeignProjects(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"foreign", "missing"} {
		_, err := e.svc.EnsureSupervisor(context.Background(), owner, p)
		wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
	}
}

func TestStandingTaskShapeIsEnforced(t *testing.T) {
	e := newEnv(t)
	e.standing("sup-s")
	plain := e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p1"})

	// Given a project with a standing supervisor, and an ordinary task beside it
	// When anything tries to record a second one, or a standing task of another shape
	for _, stmt := range []string{
		`INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, standing)
		 VALUES ('dup', 'owner-1', 'p1', 'dup', 't', 'b', 'coordination', 'claude', 'default', true)`,
		`INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, standing)
		 VALUES ('noproj', 'owner-1', NULL, 'noproj', 't', 'b', 'coordination', 'claude', 'default', true)`,
		`UPDATE tasks SET status = 'waiting', reason = 'idle' WHERE id = '` + plain + `'`,
	} {
		// Then the database refuses it
		if _, err := e.pool.Exec(context.Background(), stmt); err == nil {
			t.Fatalf("accepted: %s", stmt)
		}
	}
}

func TestStandingSupervisorIsNotCancelledRetriedOrReassigned(t *testing.T) {
	e := newEnv(t)
	id, _ := e.standing("sup-s")
	ctx := context.Background()
	v := e.view(e.own, id)

	// Given a running standing supervisor, Then its view offers none of cancel, retry or reassign
	for _, k := range []string{"cancel", "retry", "reassign"} {
		a := v.Actions[k]
		if a.Available || a.Reason == nil || *a.Reason != api.TaskEligibilityReasonStandingSupervisor {
			t.Fatalf("%s = %+v", k, a)
		}
	}
	if !v.Actions["follow_up"].Available {
		t.Fatal("a standing supervisor takes follow-ups")
	}

	// When the owner or main asks for one anyway, Then it is refused and nothing changes
	main := tasks.Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: "main-1"}
	for _, p := range []tasks.Principal{e.own, main} {
		_, err := e.svc.Cancel(ctx, p, action(id, v, "c-"+string(p.Role)))
		wantRefusal(t, err, 409, tasks.ReasonStandingSupervisor)
		_, err = e.svc.Retry(ctx, p, action(id, v, "r-"+string(p.Role)))
		wantRefusal(t, err, 409, tasks.ReasonStandingSupervisor)
	}
	if after := e.view(e.own, id); after.Task.Version != v.Task.Version || after.Task.Status != "running" {
		t.Fatalf("task moved: %+v", after.Task)
	}
}

func TestStandingSupervisorCompletedReportRests(t *testing.T) {
	e := newEnv(t)
	id, sup := e.standing("sup-s")
	ctx := context.Background()

	// Given a standing supervisor with a worker still running
	child := e.mustCreate(sup, tasks.CreateInput{})
	e.attempt(child, dbstate.AttemptChild, dbstate.AttemptActive, "kid-s")
	e.setStatus(child, "running", "")

	// When it reports the batch completed
	r, err := e.svc.Report(ctx, sup, report(sup, "done-1", dbstate.OutcomeCompleted))

	// Then it is at rest (waiting/idle), its attempt stays active, and main hears the report
	if err != nil || r.Status != "waiting" {
		t.Fatalf("completed = %+v, %v", r, err)
	}
	v := e.view(e.own, id)
	if v.Task.Reason == nil || *v.Task.Reason != "idle" || v.Attempts[0].State != "active" {
		t.Fatalf("task = %s/%v, attempt %s", v.Task.Status, v.Task.Reason, v.Attempts[0].State)
	}
	if n := e.count(`SELECT count(*) FROM task_event_deliveries d JOIN task_reports r ON r.id = d.report_id
	                 WHERE r.task_id = $1 AND d.recipient_key = 'main'`, id); n != 1 {
		t.Fatalf("main deliveries = %d", n)
	}

	// And the settle and complete sweeps leave it alone
	for _, sw := range []func() error{
		func() error { return e.svc.SettleSweep().Run(ctx) }, func() error { return e.svc.CompleteSweep().Run(ctx) },
	} {
		if err := sw(); err != nil {
			t.Fatal(err)
		}
	}
	if after := e.view(e.own, id); after.Task.Status != "waiting" || after.Attempts[0].State != "active" {
		t.Fatalf("settled a standing supervisor: %s, attempt %s", after.Task.Status, after.Attempts[0].State)
	}

	// When it reports progress again (a message woke it), Then it is running again
	if r, err := e.svc.Report(ctx, sup, report(sup, "p-1", dbstate.OutcomeProgress)); err != nil || r.Status != "running" {
		t.Fatalf("progress after rest = %+v, %v", r, err)
	}

	// And a failed report is refused: it reports blocked instead
	_, err = e.svc.Report(ctx, sup, report(sup, "f-1", dbstate.OutcomeFailed))
	wantRefusal(t, err, 422, tasks.ReasonStandingOutcome)
}

func TestStandingBriefCarriesItsGuidance(t *testing.T) {
	if !strings.Contains(tasks.StandingGuidance, "never complete") || !strings.Contains(tasks.StandingBrief("o/p1"), "o/p1") {
		t.Fatal("standing guidance or brief lost its substance")
	}
}
