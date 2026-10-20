package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

const mergedHead = "0123456789012345678901234567890123456789"

// mergedPR is the task's own PR on branch as GitHub shows it after a merge.
func mergedPR(number int, branch string) tasks.Observation {
	return tasks.Observation{
		ObservedAt: time.Now(), PRNumber: number, PRURL: "https://github.com/o/p1/pull/7", PRHeadRef: branch,
		PRRepo: "o/p1", PRHeadSHA: mergedHead, PRState: "merged", CIState: "success", CIHeadSHA: mergedHead,
	}
}

// merged records what the merge service leaves for a PR it merged on the session's call: a merged
// candidate and its proposal.
func (r *rig) merged(id, session string, number int) {
	r.t.Helper()
	r.exec(`INSERT INTO merge_candidates (id, idempotency_key, owner_id, project_id, repository_id, repository, pr_number,
	        head_sha, route, state, merge_sha)
	        VALUES ($1, 'merge:'||$1, $2, 'p1', 1, 'O/P1', $3, $4, 'auto', 'merged', repeat('c', 40))`, id, owner, number, mergedHead)
	r.exec(`INSERT INTO merge_proposals (id, candidate_id, owner_id, session_id, facts, presentation, summary_digest)
	        VALUES ('prop-'||$1, $1, $2, $3, '{}', '{}', repeat('d', 64))`, id, owner, session)
}

// reportedCode is a code task whose agent reported completed, with its PR observed merged.
func (r *rig) reportedCode(branch string) (id, attempt, binding string) {
	r.t.Helper()
	r.run()
	_, sup := r.supervisor()
	id = r.codeChild(sup, branch)
	r.eventually("child", func() bool { return r.opState(id) == "completed" })
	attempt, binding = r.attemptOf(id)
	kid := r.agent(dbstate.RoleChild, binding)
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "done", dbstate.OutcomeCompleted)); err != nil {
		r.t.Fatal(err)
	}
	r.obs.set(mergedPR(7, branch), nil)
	r.project()
	return id, attempt, binding
}

// A code task whose own PR the merge service merged completes, and its attempt then settles: the
// runtime is told to stop, the branch claim and the capacity slot are released.
func TestMergedPRCompletesTheCodeTaskAndFreesItsAttempt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id, attempt, binding := r.reportedCode("feat")
	r.merged("m1", binding, 7)
	// The child's credentials are live until it settles; another binding's must stay live.
	for _, c := range []struct{ id, binding, hash string }{{"cred-kid", binding, "a"}, {"cred-other", "other-binding", "b"}} {
		r.exec(`INSERT INTO credentials (id, kind, binding_id, token_hash, secret_name) VALUES ($1, 'mcp', $2, $3, $1)`,
			c.id, c.binding, strings.Repeat(c.hash, 64))
	}

	r.sweep(r.svc.CompleteSweep())
	v := r.view(r.own, id)
	if v.Task.Status != "completed" || v.Task.Reason != nil {
		t.Fatalf("task = %s / %v; want completed", v.Task.Status, v.Task.Reason)
	}
	if p := v.Projection; p.MergeState == nil || *p.MergeState != "merged" || p.PrState != "merged" ||
		p.MergeProposalId == nil || *p.MergeProposalId != "prop-m1" {
		t.Fatalf("projection = %+v; want merged with proposal prop-m1", p)
	}
	if r.updates(id) == 0 || r.str(`SELECT merge_proposal_id FROM tasks WHERE id = $1`, id) != "prop-m1" {
		t.Fatal("completion was not stored and published")
	}

	// The attempt is still live for merge status until the settle sweep ends it.
	if got := r.attemptState(id); got != "active" {
		t.Fatalf("attempt = %s before settlement", got)
	}
	for range 3 {
		r.sweep(r.svc.SettleSweep())
	}
	if got := r.attemptState(id); got != "completed" {
		t.Fatalf("attempt = %s; want completed", got)
	}
	if r.ses.completeCountOf(binding) == 0 {
		t.Fatal("the child's session was not completed")
	}
	if r.ses.cancelCountOf(binding) != 0 {
		t.Fatal("the child's session was cancelled instead of completed")
	}
	if got := r.str(`SELECT status FROM sessions WHERE id = $1`, binding); got != "completed" {
		t.Fatalf("session status = %s; want completed", got)
	}
	live := func(b string) int {
		return r.count(`SELECT count(*) FROM credentials WHERE binding_id = $1 AND state IN ('planned', 'published')`, b)
	}
	if live(binding) != 0 || live("other-binding") != 1 {
		t.Fatalf("live credentials: child %d, other %d; want 0 and 1", live(binding), live("other-binding"))
	}
	if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE attempt_id = $1 AND held`, attempt); n != 0 {
		t.Fatal("branch claim still held")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND capacity_held`, attempt); n != 0 {
		t.Fatal("slot still held")
	}

	// A repeat changes nothing: no new version, no new event.
	version, updates := v.Task.Version, r.updates(id)
	r.sweep(r.svc.CompleteSweep())
	if again := r.view(r.own, id); int(again.Task.Version) != int(version) || r.updates(id) != updates {
		t.Fatal("the sweep repeated the completion")
	}
}

// Only the merge service's settlement of the task's own PR completes it. Anything else leaves the
// task waiting, while the projection still shows what GitHub shows.
func TestMergedPRWithoutTheMergeServicesSettlementDoesNotComplete(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(r *rig, binding string){
		"no candidate: merged by someone else": func(r *rig, _ string) {},
		"another session's candidate":          func(r *rig, _ string) { r.merged("m1", "someone-else", 7) },
		"another PR number":                    func(r *rig, b string) { r.merged("m1", b, 8) },
		"another head": func(r *rig, b string) {
			r.merged("m1", b, 7)
			r.exec(`UPDATE merge_candidates SET head_sha = repeat('e', 40)`)
		},
		"another repository": func(r *rig, b string) {
			r.merged("m1", b, 7)
			r.exec(`UPDATE merge_candidates SET repository = 'o/p2'`)
		},
		"another project": func(r *rig, b string) {
			r.merged("m1", b, 7)
			r.exec(`UPDATE merge_candidates SET project_id = 'p2'`)
		},
		"another owner": func(r *rig, b string) {
			r.merged("m1", b, 7)
			r.exec(`UPDATE merge_candidates SET owner_id = 'someone-else'`)
		},
		"candidate not merged": func(r *rig, b string) {
			r.merged("m1", b, 7)
			r.exec(`UPDATE merge_candidates SET state = 'blocked', merge_sha = NULL`)
		},
		"PR not observed merged": func(r *rig, b string) {
			r.merged("m1", b, 7)
			open := mergedPR(7, "feat")
			open.PRState = "open"
			r.obs.set(open, nil)
			r.project()
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			id, _, binding := r.reportedCode("feat")
			setup(r, binding)
			r.sweep(r.svc.CompleteSweep())
			v := r.view(r.own, id)
			if v.Task.Status != "waiting" || v.Task.Reason == nil || *v.Task.Reason != "publication" ||
				(v.Projection.MergeProposalId != nil) {
				t.Fatalf("task = %s / %v, proposal %v; want waiting / publication", v.Task.Status, v.Task.Reason, v.Projection.MergeProposalId)
			}
			if got := r.attemptState(id); got != "active" {
				t.Fatalf("attempt = %s", got)
			}
		})
	}
}

// The projection shows a merge by anyone as merged, and says nothing for a PR that is not.
func TestProjectionDerivesMergeStateFromTheObservedPR(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.projected()
	for state, want := range map[string]string{"merged": "merged", "open": "", "closed": ""} {
		obs := mergedPR(7, "feature")
		obs.PRState = state
		r.obs.set(obs, nil)
		r.project()
		got := r.view(r.own, id).Projection
		if (want == "") != (got.MergeState == nil) || (want != "" && *got.MergeState != want) {
			t.Fatalf("pr %s: merge_state = %v; want %q", state, got.MergeState, want)
		}
		if got.MergeProposalId != nil {
			t.Fatalf("pr %s: proposal = %v", state, got.MergeProposalId)
		}
	}
}

// A task that has not reported completed is not completed by a merge: the agent that merged is
// still working, and stopping it would lose the report.
func TestMergedPRDoesNotCompleteARunningTask(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	id := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(id) == "completed" })
	_, binding := r.attemptOf(id)
	r.obs.set(mergedPR(7, "feat"), nil)
	r.project()
	r.merged("m1", binding, 7)
	r.sweep(r.svc.CompleteSweep())
	if got := r.taskStatus(id); got != "running" {
		t.Fatalf("status = %s; want running", got)
	}
}
