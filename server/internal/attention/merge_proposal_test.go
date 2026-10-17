package attention_test

import (
	"context"
	"testing"
	"time"
)

// merge writes the full approval-route chain OpenMergeProposalAttention reads: a project, a
// session with its binding, the HITL request the owner would answer, the observer's association of
// that request with the proposal, and the candidate and proposal themselves. It returns the
// proposal id, which the resulting attention item's id ("merge:"+id) and detail carry.
func (e *env) merge(t *testing.T, id string, prNumber int) {
	t.Helper()
	proj, sess, binding, req, cand := "p-"+id, "s-"+id, "k-"+id, "r-"+id, "c-"+id
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', 'n', 'o/n', 'https://x')`, proj, owner)
	e.exec(t, `INSERT INTO sessions (id, user_id, project_id, title, description, prompt) VALUES ($1, $2, $3, 't', 'd', 'p')`, sess, owner, proj)
	e.exec(t, `INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ($1, 'claude', 'agent', $2)`, sess, binding)
	e.exec(t, `INSERT INTO hitl_requests (id, owner_id, session_id, kagent_session_id, agent_namespace, agent_name, context_id, task_id,
		status_message_id, request_hash, kind, payload) VALUES ($1, $2, $3, $4, 'ns', 'ag', 'ctx', 'a2a-task', 'sm', 'h', 'tool_approval', '{}')`,
		req, owner, sess, binding)
	e.exec(t, `INSERT INTO merge_candidates (id, idempotency_key, owner_id, project_id, repository_id, repository, pr_number, head_sha, route)
		VALUES ($1, $1, $2, $3, 1, 'o/n', $4, repeat('a', 40), 'approval')`, cand, owner, proj, prNumber)
	e.exec(t, `INSERT INTO merge_proposals (id, candidate_id, owner_id, session_id, facts, presentation, summary_digest)
		VALUES ($1, $2, $3, $4, '{}', '{}', repeat('a', 64))`, id, cand, owner, sess)
	e.exec(t, `INSERT INTO hitl_merge_tools (request_id, tool_id, proposal_id) VALUES ($1, 'tool1', $2)`, req, id)
}

// A proposal superseded by a newer prepare for the same pull request already carries its own,
// durable candidate state change; the sweep must pick that up and drop the card.
func TestMergeProposalAttentionResolvesASupersededCandidate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.merge(t, "m1", 7)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || snap.Items[0].Id != "merge:m1" {
		t.Fatalf("before: %+v", snap)
	}
	e.exec(t, `UPDATE merge_candidates SET state = 'superseded' WHERE id = 'c-m1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 0 {
		t.Fatalf("a superseded candidate's card must drop: %+v", snap)
	}
}

// A candidate that is merged or closed on GitHub without ever going through Mainloop's own merge
// flow stays 'prepared' forever: nothing claims a prepared row to ask GitHub again on its own. A
// task's own, independently polled pr_state for that exact repository and PR number is the durable
// signal the sweep uses instead, with no GitHub call of its own. Once resolved, a sweep that finds
// nothing changed must write nothing and publish no further inbox:updated: the opener and the
// resolver share one eligibility rule, so an externally settled proposal does not reopen and
// re-resolve on every pass.
func TestMergeProposalAttentionResolvesAnExternallySettledPullRequest(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.merge(t, "m1", 7)
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 {
				t.Fatalf("before: %+v", snap)
			}
			e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status,
				pr_number, pr_repository, pr_head_sha, pr_state, projected_at) VALUES ('t-pr', $1, 't-pr', 'Ship it', 'brief', 'coordination', 'p',
				'explicit', 'completed', 7, 'o/n', repeat('a', 40), $2, now())`, owner, state)
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			if snap, _ := e.svc.Snapshot(ctx); snap.Count != 0 {
				t.Fatalf("a %s pull request's card must drop even while its candidate stays prepared: %+v", state, snap)
			}

			var updated time.Time
			if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE source_kind = 'merge_proposal'`).Scan(&updated); err != nil {
				t.Fatal(err)
			}
			before := len(e.inboxEvents(t))
			for range 2 {
				if err := e.svc.SyncTasks(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var after time.Time
			if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE source_kind = 'merge_proposal'`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !updated.Equal(after) || before != len(e.inboxEvents(t)) {
				t.Fatal("an externally settled proposal churned rows or events on an unchanged sweep")
			}
		})
	}
}

// Only a current observation may settle a proposal. A closed or merged snapshot taken before the
// proposal's own candidate was prepared predates the proposal and cannot speak to it, and once the
// pull request reopens at the same head, a fresher open observation always wins over an older
// closed one, even if that older snapshot was itself taken after the candidate was prepared.
func TestMergeProposalAttentionIgnoresAStaleClosedObservationAfterASameHeadReopen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.merge(t, "m1", 7)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}

	insertTask := func(id, status, prState, offset string) {
		e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status,
			pr_number, pr_repository, pr_head_sha, pr_state, projected_at)
			SELECT $1, $2, $1, 'Ship it', 'brief', 'coordination', 'p', 'explicit', $3, 7, 'o/n', repeat('a', 40), $4, created_at + $5::interval
			FROM merge_candidates WHERE id = 'c-m1'`, id, owner, status, prState, offset)
	}

	// A task observed the pull request closed before this proposal was even prepared: too old to
	// trust, so the card must stay.
	insertTask("t-old", "completed", "closed", "-1 hour")
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || snap.Items[0].Id != "merge:m1" {
		t.Fatalf("a closed snapshot older than preparation must not settle the proposal: %+v", snap)
	}

	// The pull request is reopened at the same head and a fresher task observes it open, after an
	// otherwise-current closed snapshot that alone would have settled the proposal: the newer, open
	// observation must still win.
	insertTask("t-closed-current", "completed", "closed", "1 minute")
	insertTask("t-reopened", "running", "open", "2 minutes")
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || snap.Items[0].Id != "merge:m1" {
		t.Fatalf("a newer open observation must win over a stale closed one: %+v", snap)
	}
}

// A task's merged/closed observation at a different head than the one the candidate names must not
// resolve the card: that head is a later (or earlier) generation of the pull request, not the one
// this proposal is about, so the signal is not trusted.
func TestMergeProposalAttentionIgnoresAMismatchedHead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.merge(t, "m1", 7)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status,
		pr_number, pr_repository, pr_head_sha, pr_state) VALUES ('t-pr', $1, 't-pr', 'Ship it', 'brief', 'coordination', 'p', 'explicit',
		'completed', 7, 'o/n', repeat('b', 40), 'closed')`, owner)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || snap.Items[0].Id != "merge:m1" {
		t.Fatalf("a mismatched head must not resolve the card: %+v", snap)
	}
}

// A proposal still awaiting the owner, for a pull request no task has observed settled, stays open
// across repeated sweeps.
func TestMergeProposalAttentionLiveProposalStays(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.merge(t, "m1", 7)
	for range 3 {
		if err := e.svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
		if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || snap.Items[0].Id != "merge:m1" {
			t.Fatalf("a live approval-route proposal must stay: %+v", snap)
		}
	}
}
