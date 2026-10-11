package tasks_test

import (
	"fmt"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestProjectionReadMasksUnfreshCI(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	for _, state := range []string{"success", "pending", "failure"} {
		for _, tc := range []struct {
			name, update, want string
		}{
			{"fresh", "projected_at = now() - interval '4 minutes'", state},
			{"stale", "projected_at = now() - interval '6 minutes'", "unknown"},
			{"future", "projected_at = now() + interval '1 minute'", "unknown"},
			{"missing_time", "projected_at = NULL", "unknown"},
			{"mismatched_head", "ci_head_sha = 'other'", "unknown"},
			{"missing_ci_head", "ci_head_sha = NULL", "unknown"},
			{"missing_pr_head", "pr_head_sha = NULL", "unknown"},
			{"empty_pr_head", "pr_head_sha = '', ci_head_sha = ''", "unknown"},
		} {
			t.Run(state+"/"+tc.name, func(t *testing.T) {
				r.obs.set(tasks.Observation{
					PRNumber: 7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "a", PRState: "open", CIState: state, CIHeadSHA: "a",
				}, nil)
				r.project()
				// Only test fixtures change the stored observation; the read must neither write
				// the task nor call GitHub to repair it.
				r.exec(fmt.Sprintf("UPDATE tasks SET %s WHERE id = $1", tc.update), id)
				before := r.view(r.own, id)
				calls, events := r.obs.calls, r.updates(id)
				got := r.view(r.own, id)
				if string(got.Projection.CiState) != tc.want || string(got.Projection.PrState) != "open" {
					t.Fatalf("read projection = %+v; want CI %s", got.Projection, tc.want)
				}
				if got.Task.Version != before.Task.Version || r.obs.calls != calls || r.updates(id) != events {
					t.Fatal("reading freshness changed the task or called GitHub")
				}
			})
		}
	}
}

func TestProjectionUnchangedObservationRefreshesFreshness(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.set(tasks.Observation{
		PRNumber: 7, PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: "a", PRState: "open", CIState: "success", CIHeadSHA: "a",
	}, nil)
	r.project()
	version, events := r.view(r.own, id).Task.Version, r.updates(id)
	r.exec("UPDATE tasks SET projected_at = now() - interval '6 minutes' WHERE id = $1", id)
	if got := r.view(r.own, id).Projection; string(got.CiState) != "unknown" {
		t.Fatalf("stale CI = %+v", got)
	}
	r.sweep(r.svc.ProjectionSweep())
	got := r.view(r.own, id)
	if string(got.Projection.CiState) != "success" || got.Task.Version != version || r.updates(id) != events {
		t.Fatalf("unchanged refresh = %+v, version %d, events %d", got.Projection, got.Task.Version, r.updates(id))
	}
}
