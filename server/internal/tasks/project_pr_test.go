package tasks_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

// branchPR is a PR for the task's branch as GitHub shows it.
func branchPR(number int, state string) tasks.Observation {
	sha := fmt.Sprintf("%040d", number)
	return tasks.Observation{
		ObservedAt: time.Now(), PRNumber: number, PRURL: fmt.Sprintf("https://github.com/o/p1/pull/%d", number),
		PRHeadRef: "feature", PRRepo: "o/p1", PRHeadSHA: sha, PRState: state, CIState: "success", CIHeadSHA: sha,
	}
}

// created records a finished PR creation by the session, as the creation reconciler leaves it.
func (r *rig) created(id, session string, number int, createdAt string) {
	r.t.Helper()
	r.exec(`INSERT INTO pr_creations (id, idempotency_key, owner_id, project_id, session_id, request_id, payload_hash,
	        repo_id, repo_full_name, head, base, expected_sha, title, state, pr_number, url, created_at)
	        VALUES ($1, 'pr:'||$1, $2, 'p1', NULLIF($3, ''), $1, repeat('a', 64), 1, 'o/p1', 'feature', 'main',
	        repeat('b', 40), 'T', 'created', $4::bigint, 'https://github.com/o/p1/pull/'||$4::text, $5::timestamptz)`,
		id, owner, session, number, createdAt)
}

func (r *rig) lastExpected() int {
	r.t.Helper()
	if len(r.obs.expected) == 0 {
		r.t.Fatal("the observer was not called")
	}
	return r.obs.expected[len(r.obs.expected)-1]
}

func (r *rig) prNumber(id string) int {
	r.t.Helper()
	if n := r.view(r.own, id).Projection.PrNumber; n != nil {
		return *n
	}
	return 0
}

func TestProjectionLinksThePRTheAttemptCreated(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	// Someone else's PR on the branch is open and has a higher number; the attempt's own is lower.
	r.obs.branch = []tasks.Observation{branchPR(4, "open"), branchPR(9, "open"), branchPR(12, "closed")}
	r.created("c1", "kid-p", 4, "2026-01-01")
	r.project()
	if got := r.prNumber(id); got != 4 || r.lastExpected() != 4 {
		t.Fatalf("linked PR %d, asked for %d; want the attempt's PR 4", got, r.lastExpected())
	}
}

func TestProjectionTakesTheNewestCreation(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.branch = []tasks.Observation{branchPR(4, "closed"), branchPR(6, "open")}
	r.created("c1", "kid-p", 4, "2026-01-01")
	r.created("c2", "kid-p", 6, "2026-01-02")
	r.project()
	if got := r.prNumber(id); got != 6 {
		t.Fatalf("linked PR %d; want the newest creation 6", got)
	}
}

func TestProjectionWithoutACreationPicksAsBefore(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.branch = []tasks.Observation{branchPR(4, "closed"), branchPR(9, "open"), branchPR(12, "closed")}
	r.project()
	if got := r.prNumber(id); got != 9 || r.lastExpected() != 0 {
		t.Fatalf("linked PR %d, asked for %d; want the branch's open PR 9 and no expectation", got, r.lastExpected())
	}
}

func TestProjectionIgnoresCreationsThatAreNotTheAttempts(t *testing.T) {
	for name, setup := range map[string]func(r *rig){
		"another session": func(r *rig) { r.created("c1", "someone-else", 4, "2026-01-01") },
		"no session":      func(r *rig) { r.created("c1", "", 4, "2026-01-01") },
		"other project": func(r *rig) {
			r.created("c1", "kid-p", 4, "2026-01-01")
			r.exec(`UPDATE pr_creations SET project_id = 'p2'`)
		},
		"other head": func(r *rig) {
			r.created("c1", "kid-p", 4, "2026-01-01")
			r.exec(`UPDATE pr_creations SET head = 'other'`)
		},
		"other owner": func(r *rig) {
			r.created("c1", "kid-p", 4, "2026-01-01")
			r.exec(`UPDATE pr_creations SET owner_id = 'someone-else'`)
		},
		"not created": func(r *rig) {
			r.created("c1", "kid-p", 4, "2026-01-01")
			r.exec(`UPDATE pr_creations SET state = 'uncertain', pr_number = NULL, url = NULL`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			id := r.projected()
			r.obs.branch = []tasks.Observation{branchPR(4, "closed"), branchPR(9, "open")}
			setup(r)
			r.project()
			if got := r.prNumber(id); got != 9 || r.lastExpected() != 0 {
				t.Fatalf("linked PR %d, asked for %d; want the open PR 9 and no expectation", got, r.lastExpected())
			}
		})
	}
}

func TestProjectionReportsTheCreatedPRClosedOrMissing(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	// The attempt's PR was closed and another is open: the closed one is reported, not swapped.
	r.obs.branch = []tasks.Observation{branchPR(4, "closed"), branchPR(9, "open")}
	r.created("c1", "kid-p", 4, "2026-01-01")
	r.project()
	if p := r.view(r.own, id).Projection; p.PrNumber == nil || *p.PrNumber != 4 || string(p.PrState) != "closed" {
		t.Fatalf("projection = %+v; want PR 4 closed", p)
	}
	// Now the PR is gone from the branch: no PR, and still not the other one.
	r.obs.branch = []tasks.Observation{branchPR(9, "open")}
	r.project()
	if p := r.view(r.own, id).Projection; p.PrNumber != nil || string(p.PrState) != "unknown" {
		t.Fatalf("projection = %+v; want no PR", p)
	}
}

func TestProjectionNewAttemptDoesNotInheritThePreviousAttemptsPR(t *testing.T) {
	r := newRig(t)
	id := r.projected()
	r.obs.branch = []tasks.Observation{branchPR(4, "open"), branchPR(9, "open")}
	r.created("c1", "kid-p", 4, "2026-01-01")
	r.project()
	if got := r.prNumber(id); got != 4 {
		t.Fatalf("first attempt linked PR %d; want 4", got)
	}
	// A second attempt on the same task, with its own binding and no creation of its own.
	r.exec(`UPDATE task_attempts SET state = 'superseded', capacity_held = false WHERE task_id = $1`, id)
	r.exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
	        agent_namespace, agent_name, role, depth, state, binding_id, session_id, workspace_id, capacity_held)
	        SELECT 'attempt-2', task_id, 2, profile_id, native_provider, configuration_revision, agent_namespace,
	               agent_name, role, depth, 'active', 'kid-p2', 'kid-p2', 'kid-p2', true
	        FROM task_attempts WHERE task_id = $1`, id)
	r.exec(`UPDATE tasks SET current_attempt_id = 'attempt-2' WHERE id = $1`, id)
	r.project()
	if got := r.lastExpected(); got != 0 {
		t.Fatalf("the new attempt was asked for PR %d; want no expectation", got)
	}
	if got := r.prNumber(id); got != 9 {
		t.Fatalf("new attempt linked PR %d; want today's pick, the open PR 9", got)
	}
	// Once it opens its own PR, that one is linked.
	r.created("c2", "kid-p2", 4, "2026-01-02")
	r.project()
	if got := r.prNumber(id); got != 4 || r.lastExpected() != 4 {
		t.Fatalf("new attempt linked PR %d, asked for %d; want its own 4", got, r.lastExpected())
	}
}
