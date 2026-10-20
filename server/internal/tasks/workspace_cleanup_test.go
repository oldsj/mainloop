package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// Model cancelled creates whose processes disappeared before recording their return.
func seedWorkspaceCleanup(r *rig, i, inflight int, age string) (taskID, attempt, key string) {
	r.t.Helper()
	taskID = fmt.Sprintf("review-cleanup-task-%d", i)
	attempt = uuid.NewString()
	key = tasks.WorkspaceRequestID(attempt)
	r.exec(`INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief,
		mode, assigned_profile_id, selection_source, status, checkout_branch, checkout_ref, checkout_depth)
		VALUES ($1, $2, 'p1', $1, 'review', 'review', 'code', 'claude', 'explicit', 'cancelled', $3, '', 0)`,
		taskID, owner, fmt.Sprintf("review-branch-%d", i))
	r.exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider,
		configuration_revision, agent_namespace, agent_name, role, depth, state,
		capacity_held, stop_state, workspace_request_id, workspace_creates_inflight, updated_at)
		VALUES ($1, $2, 1, 'claude', 'claude', 'v', 'kagent', 'a', 'supervisor', 1,
		'cancelled', false, 'confirmed', $3, $4, now() - $5::interval)`,
		attempt, taskID, key, inflight, age)
	r.exec(`UPDATE tasks SET current_attempt_id = $1 WHERE id = $2`, attempt, taskID)
	return taskID, attempt, key
}

// More than two full batches of unresolved reservations cannot keep newer cleanup or
// the runtime stop resolver from making progress. Ported from the T1 review probe.
func TestReviewT1UnresolvedCleanupDoesNotStarveLateWorkspace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	const stuck = 125
	for i := range stuck {
		seedWorkspaceCleanup(r, i, 1, "2 hours")
	}
	_, attempt, key := seedWorkspaceCleanup(r, stuck, 0, "1 hour")
	lc, err := r.ws.CreateWithRequestID(context.Background(), key, api.CreateWorkspaceRequest{
		ProjectId: ptr("p1"), Branch: ptr(fmt.Sprintf("review-branch-%d", stuck)), AgentKind: ptr("claude"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// An aborted create whose task failed before the sweep confirmed its stop and fenced it.
	draining := uuid.NewString()
	r.exec(`INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief,
		mode, assigned_profile_id, selection_source, status, checkout_branch, checkout_ref, checkout_depth)
		VALUES ('review-draining-task', $1, 'p1', 'review-draining-task', 'review', 'review',
		'code', 'claude', 'explicit', 'failed', 'review-draining-branch', '', 0)`, owner)
	r.exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider,
		configuration_revision, agent_namespace, agent_name, role, depth, state, capacity_held,
		stop_state, workspace_request_id, writer_generation, updated_at)
		VALUES ($1, 'review-draining-task', 1, 'claude', 'claude', 'v', 'kagent', 'a',
		'supervisor', 1, 'draining', true, 'requested', $2, 1, now() - interval '1 hour')`,
		draining, tasks.WorkspaceRequestID(draining))
	r.exec(`UPDATE tasks SET current_attempt_id = $1 WHERE id = 'review-draining-task'`, draining)
	r.exec(`INSERT INTO workspace_writer_claims (owner_id, repository, branch, generation, attempt_id, held)
		VALUES ($1, 'o/p1', 'review-draining-branch', 1, $2, true)`, owner, draining)
	before := r.view(r.own, "review-draining-task")
	cancel, err := r.svc.Cancel(context.Background(), r.own, action("review-draining-task", before, uuid.NewString()))
	if err != nil || cancel.State != "completed" || r.view(r.own, "review-draining-task").Task.Version != before.Task.Version {
		t.Fatalf("cancel of failed task should record a no-op: %+v, %v", cancel, err)
	}
	r.sweep(r.svc.SettleSweep())
	if slot, claim := r.held(draining); slot != 0 || claim != 0 || r.attemptState("review-draining-task") != "failed" {
		t.Fatal("final workspace cleanup starved runtime stop resolution")
	}
	// Stop resolution progresses on the first pass even though cleanup needs three batches.
	for range 3 {
		r.sweep(r.svc.SettleSweep())
	}
	if r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, lc.WorkspaceId) != 0 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND workspace_cleanup_confirmed`, attempt) != 1 {
		t.Fatal("unresolved cleanup monopolized its batch and leaked the later workspace")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE workspace_creates_inflight = 1
		AND NOT workspace_cleanup_confirmed AND workspace_cleanup_attempts > 0 AND workspace_cleanup_after IS NOT NULL`); n != stuck {
		t.Fatalf("unresolved reservations must be preserved and durably deferred: got %d, want %d", n, stuck)
	}
}

func TestUnresolvedWorkspaceCleanupResumesAfterCreateReturns(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	taskID, attempt, key := seedWorkspaceCleanup(r, 0, 1, "2 hours")
	r.sweep(r.svc.SettleSweep())
	before := r.view(r.own, taskID)
	in := action(taskID, before, "cancel-final")
	cancel, err := r.svc.Cancel(context.Background(), r.own, in)
	if err != nil || cancel.State != "completed" || r.view(r.own, taskID).Task.Version != before.Task.Version {
		t.Fatalf("cancel of final attempt should record a no-op: %+v, %v", cancel, err)
	}
	replay, err := r.svc.Cancel(context.Background(), r.own, in)
	if err != nil || replay.Id != cancel.Id {
		t.Fatalf("cancel no-op replay = %+v, %v", replay, err)
	}
	// The next look is durable across a service restart, even though cancellation is completed.
	restarted := *r.svc
	r.sweep(restarted.SettleSweep())
	if r.count(`SELECT workspace_cleanup_attempts FROM task_attempts WHERE id = $1`, attempt) != 1 ||
		r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE id = $1`, attempt) != 1 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND workspace_cleanup_confirmed`, attempt) != 0 {
		t.Fatal("restart ignored backoff or prematurely resolved the reservation")
	}
	lc, err := r.ws.CreateWithRequestID(context.Background(), key, api.CreateWorkspaceRequest{
		ProjectId: ptr("p1"), Branch: ptr("review-branch-0"), AgentKind: ptr("claude"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model the reserved call finally returning, but its handler dying before it cleaned up.
	if err := dbq.New(r.pool).FinishAttemptWorkspaceCreate(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	r.sweep(restarted.SettleSweep())
	if r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, lc.WorkspaceId) != 0 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND workspace_cleanup_confirmed`, attempt) != 1 {
		t.Fatal("resolved reservation did not wake cleanup of its late workspace")
	}
	assertRemoved := len(r.ws.removed)
	r.sweep(restarted.SettleSweep())
	if len(r.ws.removed) != assertRemoved {
		t.Fatal("confirmed cleanup repeated deletion")
	}
}

func TestWorkspaceCleanupBackoffPreservesUncertainReservations(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	_, attempt, key := seedWorkspaceCleanup(r, 0, 1, "2 hours")
	// Simulate scheduled time passing without sleeping; absence is never proof of completion.
	for i, delay := range []int{5, 10, 20, 40, 80, 160, 300, 300} {
		r.exec(`UPDATE task_attempts SET workspace_cleanup_after = now() - interval '1 second' WHERE id = $1`, attempt)
		r.sweep(r.svc.SettleSweep())
		var remaining float64
		if err := r.pool.QueryRow(context.Background(), `SELECT extract(epoch FROM (workspace_cleanup_after - now()))::float8
			FROM task_attempts WHERE id = $1`, attempt).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining <= float64(delay)-2 || remaining > float64(delay) ||
			r.count(`SELECT workspace_cleanup_attempts FROM task_attempts WHERE id = $1`, attempt) != i+1 ||
			r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE id = $1`, attempt) != 1 ||
			r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND workspace_cleanup_confirmed`, attempt) != 0 {
			t.Fatalf("look %d: delay = %fs, want about %ds with reservation still held", i+1, remaining, delay)
		}
	}
	// A late workspace is removed on the next due look while absence remains uncertain.
	lc, err := r.ws.CreateWithRequestID(context.Background(), key, api.CreateWorkspaceRequest{
		ProjectId: ptr("p1"), Branch: ptr("review-branch-0"), AgentKind: ptr("claude"),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ws.deleteErr = &workspaces.UnconfirmedError{Detail: "delete still in flight"}
	r.exec(`UPDATE task_attempts SET workspace_cleanup_after = now() - interval '1 second' WHERE id = $1`, attempt)
	var unconfirmed *workspaces.UnconfirmedError
	if err := r.svc.SettleSweep().Run(context.Background()); !errors.As(err, &unconfirmed) {
		t.Fatalf("unconfirmed deletion = %v", err)
	}
	if r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND workspace_cleanup_attempts = 9
		AND workspace_cleanup_after > now() AND NOT workspace_cleanup_confirmed AND workspace_creates_inflight = 1`, attempt) != 1 {
		t.Fatal("unconfirmed deletion did not durably defer unresolved cleanup")
	}
	r.ws.deleteErr = nil
	r.sweep(r.svc.SettleSweep())
	if r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, lc.WorkspaceId) != 1 {
		t.Fatal("sweep retried an unconfirmed delete before its backoff was due")
	}
	r.exec(`UPDATE task_attempts SET workspace_cleanup_after = now() - interval '1 second' WHERE id = $1`, attempt)
	r.sweep(r.svc.SettleSweep())
	if r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, lc.WorkspaceId) != 0 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND NOT workspace_cleanup_confirmed
		AND workspace_creates_inflight = 1 AND workspace_cleanup_attempts = 10`, attempt) != 1 {
		t.Fatal("late workspace was leaked or deletion falsely resolved the uncertain reservation")
	}
}
