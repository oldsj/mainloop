package tasks_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// projected makes a running code task with an attempt for the projection sweep to look at.
func (r *rig) projected() string {
	root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
	r.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-p")
	r.setStatus(root, "running", "")
	sup := r.agent(dbstate.RoleSupervisor, "sup-p")
	id := r.mustCreate(sup, tasks.CreateInput{Mode: dbstate.TaskModeCode, Checkout: &tasks.Checkout{Branch: "feature"}})
	r.attempt(id, dbstate.AttemptChild, dbstate.AttemptActive, "kid-p")
	r.setStatus(id, "running", "")
	return id
}

func (r *rig) project() {
	r.t.Helper()
	r.exec(`UPDATE tasks SET projected_at = NULL`)
	r.sweep(r.svc.ProjectionSweep())
}

func (r *rig) updates(id string) int {
	return r.count(`SELECT count(*) FROM events WHERE type = 'task:updated' AND payload->>'task_id' = $1`, id)
}

func TestProjectionStoresAndPublishes(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	base := r.updates(id)
	r.obs.set(tasks.Observation{
		ObservedAt: time.Now(),
		PRNumber:   7, PRURL: "https://example.test/o/p1/pull/7", PRHeadRef: "feature", PRRepo: "O/P1",
		PRHeadSHA: "abc", PRState: "open", CIState: "success", CIHeadSHA: "abc",
	}, nil)
	r.project()
	v := r.view(r.own, id)
	p := v.Projection
	if p.PrNumber == nil || *p.PrNumber != 7 || string(p.PrState) != "open" || string(p.CiState) != "success" {
		t.Fatalf("projection = %+v", p)
	}
	if v.Task.Status != "running" {
		t.Fatalf("status = %s: a projection never moves a task", v.Task.Status)
	}
	if got := r.updates(id); got != base+1 {
		t.Fatalf("task:updated events = %d, want %d", got, base+1)
	}

	// The same observation again only touches the observation time.
	version := v.Task.Version
	r.project()
	if after := r.view(r.own, id); after.Task.Version != version || r.updates(id) != base+1 {
		t.Fatalf("unchanged observation changed the task: version %d -> %d, events %d", version, after.Task.Version, r.updates(id))
	}
	if r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND projected_at IS NOT NULL`, id) != 1 {
		t.Fatal("observation time not touched")
	}
}

func TestProjectionCIMustMatchTheHead(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.set(tasks.Observation{
		ObservedAt: time.Now(),
		PRNumber:   7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "new", PRState: "open", CIState: "success", CIHeadSHA: "old",
	}, nil)
	r.project()
	if got := r.view(r.own, id).Projection; string(got.CiState) != "unknown" {
		t.Fatalf("ci for another commit = %s", got.CiState)
	}
	for _, state := range []string{"pending", "failure"} {
		r.obs.set(tasks.Observation{
			ObservedAt: time.Now(),
			PRNumber:   7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "new", PRState: "open", CIState: state, CIHeadSHA: "old",
		}, nil)
		r.project()
		if got := r.view(r.own, id).Projection; string(got.CiState) != "unknown" {
			t.Fatalf("%s for another commit = %s", state, got.CiState)
		}
	}
	r.obs.set(tasks.Observation{
		ObservedAt: time.Now(),
		PRNumber:   7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "new", PRState: "open", CIState: "failure", CIHeadSHA: "new",
	}, nil)
	r.project()
	if got := r.view(r.own, id).Projection; string(got.CiState) != "failure" {
		t.Fatalf("ci = %s", got.CiState)
	}
}

func TestProjectionIgnoresAnotherBranchOrRepo(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, o := range []tasks.Observation{
		{PRNumber: 9, PRHeadRef: "other", PRRepo: "o/p1", PRState: "open"},
		{PRNumber: 9, PRHeadRef: "feature", PRRepo: "o/p2", PRState: "open"},
	} {
		r.obs.set(o, nil)
		r.project()
		if got := r.view(r.own, id).Projection; got.PrNumber != nil || string(got.PrState) != "unknown" {
			t.Fatalf("projection took %+v: %+v", o, got)
		}
	}
}

func TestProjectionErrorsDegradeToUnknownAndNeverComplete(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.set(tasks.Observation{PRNumber: 7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "a", PRState: "merged", CIState: "success", CIHeadSHA: "a"}, nil)
	r.project()
	if got := r.view(r.own, id); string(got.Projection.PrState) != "merged" || got.Task.Status != "running" {
		t.Fatalf("merged pr: %+v status %s", got.Projection, got.Task.Status)
	}
	r.obs.set(tasks.Observation{}, errors.New("github is down"))
	r.project()
	got := r.view(r.own, id)
	if string(got.Projection.PrState) != "unknown" || string(got.Projection.CiState) != "unknown" || got.Projection.PrNumber != nil {
		t.Fatalf("projection after an error = %+v", got.Projection)
	}
	if got.Task.Status != "running" {
		t.Fatalf("status = %s", got.Task.Status)
	}
	if got.Projection.ObservedAt == nil {
		t.Fatal("a failed read has no sweep time")
	}
}

func TestProjectionSkipsFinalTasksAndTasksWithoutAttempts(t *testing.T) {
	r := newRig(t)
	done := r.projected()
	r.setStatus(done, "completed", "")
	r.codeTask() // never provisioned
	r.obs.set(tasks.Observation{PRNumber: 7, PRHeadRef: "feature", PRRepo: "o/p1", PRState: "open"}, nil)
	r.project()
	if r.obs.calls != 0 {
		t.Fatalf("observed %d times", r.obs.calls)
	}
	if r.view(r.own, done).Projection.PrNumber != nil {
		t.Fatal("a final task was projected")
	}
}

func TestProjectionRacesAFinishingTask(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.set(tasks.Observation{PRNumber: 7, PRHeadRef: "feature", PRRepo: "o/p1", PRState: "open"}, nil)
	// The task ends after the sweep listed it and before it stored the result: the write sees the
	// final task under the row lock and leaves it alone.
	r.obs.onObserve = func() { r.setStatus(id, "cancelled", "") }
	r.project()
	if r.view(r.own, id).Projection.PrNumber != nil {
		t.Fatal("a task that ended during observation was projected")
	}
}
