-- Task locks. The request key serialises operations with the same idempotency key; the task row
-- lock serialises changes to one task. Take them in that order.

-- name: LockTaskRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- Tasks

-- name: InsertTask :one
-- reason stays NULL: a new task has nothing wrong with it yet, and provisioning has not been
-- attempted. See Service.Create.
INSERT INTO tasks (
    id, owner_id, project_id, topic_id, parent_task_id, root_task_id, creator_binding_id, title, brief,
    mode, assigned_profile_id, selection_source, provider_constraint, checkout_branch, checkout_ref,
    checkout_depth
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND owner_id = $2;

-- name: GetTaskForUpdate :one
SELECT * FROM tasks WHERE id = $1 AND owner_id = $2 FOR UPDATE;

-- name: ListTasks :many
SELECT * FROM tasks
WHERE owner_id = $1
  AND (sqlc.narg(project_id)::text IS NULL OR project_id = sqlc.narg(project_id))
  AND (sqlc.narg(parent_task_id)::text IS NULL OR parent_task_id = sqlc.narg(parent_task_id))
ORDER BY created_at, id;

-- name: ListChildTasks :many
SELECT * FROM tasks WHERE owner_id = $1 AND parent_task_id = $2 ORDER BY created_at, id;

-- Move a task to a new status. The version check makes a concurrent writer lose: no row back
-- means the task changed. The owner decision is set or cleared with the status.
-- name: UpdateTaskState :one
UPDATE tasks
SET status = $3,
    reason = sqlc.narg(reason),
    decision_attempt_id = sqlc.narg(decision_attempt_id),
    decision_report_id = sqlc.narg(decision_report_id),
    decision_action = sqlc.narg(decision_action),
    version = version + 1,
    updated_at = now()
WHERE id = $1 AND version = $2
RETURNING *;

-- Attempts

-- name: GetTaskAttempt :one
SELECT * FROM task_attempts WHERE id = $1;

-- name: ListTaskAttempts :many
SELECT * FROM task_attempts WHERE task_id = ANY(sqlc.arg(task_ids)::text[]) ORDER BY task_id, number;

-- name: SetAttemptResult :exec
UPDATE task_attempts SET result_ref = $2, updated_at = now() WHERE id = $1;

-- The task principal behind an MCP binding: its attempt, the task it belongs to, whether that
-- attempt is still the task's current one, and the state of the parent's current attempt.
-- name: GetAttemptPrincipal :one
SELECT a.id AS attempt_id, a.task_id, a.state AS attempt_state, a.role AS attempt_role, a.depth AS attempt_depth,
       t.owner_id, t.status AS task_status, t.mode AS task_mode, t.project_id, t.root_task_id, t.parent_task_id,
       (coalesce(t.current_attempt_id = a.id, false))::boolean AS is_current,
       -- The attempt's runtime identity is one id (session = binding, and the workspace of a code attempt), and the
       -- session it names belongs to the task's project.
       (a.session_id IS NOT DISTINCT FROM a.binding_id
        AND a.workspace_id IS NOT DISTINCT FROM (CASE WHEN t.mode = 'code' THEN a.binding_id END)
        AND (s.id IS NULL OR s.project_id IS NOT DISTINCT FROM t.project_id))::boolean AS scope_consistent,
       coalesce(pt.status, '')::text AS parent_task_status, coalesce(pa.state, '')::text AS parent_attempt_state
FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
LEFT JOIN tasks pt ON pt.id = t.parent_task_id
LEFT JOIN task_attempts pa ON pa.id = pt.current_attempt_id
LEFT JOIN sessions s ON s.id = a.binding_id
WHERE a.binding_id = $1;

-- Locks an attempt against being moved while the caller's write commits, and reads its state.
-- name: LockTaskAttemptShared :one
SELECT * FROM task_attempts WHERE id = $1 FOR SHARE;

-- name: GetTaskCurrentAttempt :one
SELECT current_attempt_id FROM tasks WHERE id = $1;

-- Operations

-- name: InsertTaskOperation :one
INSERT INTO task_operations (
    id, owner_id, principal_key, request_id, request_digest, request_payload, kind, task_id, attempt_id,
    state, last_confirmed_step, source_attempt_id, reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING *;

-- name: GetTaskOperationByKey :one
SELECT * FROM task_operations WHERE owner_id = $1 AND principal_key = $2 AND request_id = $3;

-- name: GetTaskOperation :one
SELECT * FROM task_operations WHERE id = $1 AND owner_id = $2;

-- name: ListTaskOperations :many
SELECT * FROM task_operations WHERE task_id = ANY(sqlc.arg(task_ids)::text[]) ORDER BY task_id, created_at, id;

-- name: SetTaskOperationState :one
UPDATE task_operations
SET state = $2, last_confirmed_step = $3, reason = sqlc.narg(reason), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetTaskOperationTask :one
UPDATE task_operations SET task_id = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- Artifacts

-- name: ListTaskArtifacts :many
SELECT a.* FROM task_artifacts a
JOIN task_operations o ON o.id = a.operation_id
WHERE o.task_id = ANY(sqlc.arg(task_ids)::text[])
ORDER BY o.task_id, a.created_at, a.id;

-- Reports

-- name: InsertTaskReport :one
INSERT INTO task_reports (
    id, task_id, attempt_id, request_id, request_digest, summary, outcome, owner_action, evidence_refs, task_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: GetTaskReportByRequest :one
SELECT * FROM task_reports WHERE attempt_id = $1 AND request_id = $2;

-- name: ListTaskReports :many
SELECT * FROM task_reports WHERE task_id = ANY(sqlc.arg(task_ids)::text[]) ORDER BY task_id, created_at, id;

-- name: HasCompletedReport :one
SELECT EXISTS (SELECT 1 FROM task_reports WHERE task_id = $1 AND outcome = 'completed');

-- Event deliveries

-- name: InsertTaskEventDelivery :exec
INSERT INTO task_event_deliveries (id, report_id, recipient_key) VALUES ($1, $2, $3);

-- name: ListTaskEventDeliveries :many
SELECT * FROM task_event_deliveries WHERE report_id = $1 ORDER BY recipient_key;

-- A change that leaves the status alone but that clients should reread: a new operation or report.
-- name: TouchTask :one
UPDATE tasks SET version = version + 1, updated_at = now() WHERE id = $1 AND version = $2 RETURNING *;

-- A task cancelled before it was provisioned ends its create request with it, so the reconcile
-- loop does not provision a cancelled task.
-- name: BlockRequestedCreateOps :exec
UPDATE task_operations SET state = 'blocked', reason = NULL, updated_at = now()
WHERE task_id = $1 AND kind = 'create' AND state = 'requested';

-- An attempt that has ended no longer needs its create request driven further.
-- name: BlockCreateOpsForAttempt :exec
UPDATE task_operations
SET state = 'blocked', reason = NULL, lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE kind = 'create' AND target_attempt_id = $1 AND state IN ('target_creating', 'target_ready');

-- Provisioning and settlement. The attempt, the task's pointer to it and the writer claim change
-- in one transaction under the task row lock.

-- name: InsertTaskAttempt :one
INSERT INTO task_attempts (
    id, task_id, number, profile_id, native_provider, configuration_revision, agent_namespace, agent_name,
    role, depth, writer_generation, initial_ref, session_id, binding_id, workspace_id, workspace_request_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
)
RETURNING *;

-- name: NextAttemptNumber :one
SELECT (coalesce(max(number), 0) + 1)::int FROM task_attempts WHERE task_id = $1;

-- The native session of a code attempt, once the workspace exists. The environment is filled once.
-- name: AttachAttemptSession :one
UPDATE task_attempts
SET session_id = $2, binding_id = $2, workspace_id = $2,
    environment = coalesce(environment, sqlc.narg(environment)::jsonb), updated_at = now()
WHERE id = $1 AND state = 'creating' AND (session_id IS NULL OR session_id = $2)
RETURNING *;

-- name: SetAttemptBrief :exec
UPDATE task_attempts SET brief_delivery_id = $2, updated_at = now() WHERE id = $1;

-- Moves an attempt along an edge of the attempt table. The from state is part of the match, so a
-- concurrent move makes this return no row.
-- name: MoveAttempt :one
UPDATE task_attempts
SET state = sqlc.arg(to_state), capacity_held = sqlc.arg(capacity_held), updated_at = now(),
    -- Entering draining requests the stop in this statement, before anything is asked of kagent.
    stop_state = CASE WHEN sqlc.arg(to_state) = 'draining' THEN 'requested' ELSE stop_state END,
    evidence_refs = CASE WHEN sqlc.narg(evidence_ref)::text IS NULL THEN evidence_refs
                         ELSE array_append(evidence_refs, sqlc.narg(evidence_ref)::text) END
WHERE id = sqlc.arg(id) AND state = sqlc.arg(from_state)
RETURNING *;

-- name: ListUnsettledAttempts :many
-- Attempts that still hold a runtime although their task has ended, and attempts stuck draining or
-- fenced: the settle sweep finishes them. The idle gate keeps it from racing a handler that is
-- still working on the attempt.
SELECT a.* FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
WHERE (a.state IN ('draining', 'fenced')
       OR (a.state IN ('creating', 'active') AND t.status IN ('completed', 'failed', 'cancelled')))
  AND a.updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
  AND (a.resolve_after IS NULL OR a.resolve_after <= now())
ORDER BY a.updated_at, a.id
LIMIT $1;

-- name: ListActiveAttemptsForLiveness :many
-- The current attempt of every task that still thinks it is running: the liveness sweep checks
-- each one's kagent session and bounds how long it may go without activity or in total. A task
-- already blocked, however that happened, drops out on its own; this sweep never reopens one.
SELECT a.* FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
WHERE a.state = 'active' AND t.status = 'running' AND t.current_attempt_id = a.id
ORDER BY a.updated_at, a.id
LIMIT $1;

-- name: TouchAttemptLiveness :exec
-- Bumps updated_at alone, so a liveness look that finds nothing to do still rotates the attempt
-- behind others in ListActiveAttemptsForLiveness's order, instead of it being picked every pass.
UPDATE task_attempts SET updated_at = now() WHERE id = $1;

-- name: RecordAttemptProgress :exec
-- Stores the fingerprint of the attempt's kagent task the liveness sweep just saw change, and when.
-- It writes nothing else, so it never contends with a settle or a report on the attempt's state.
UPDATE task_attempts SET progress_marker = $2, progress_at = $3 WHERE id = $1;

-- name: RestartAttemptStallClock :exec
-- A progress report is proof of life: the stall clock counts from it, against the fingerprint the sweep
-- already holds. Without this, a report that recovers a stalled task is overridden by the next sweep,
-- which still finds the old fingerprint older than the stall timeout. Nothing is written for an attempt
-- the sweep has not fingerprinted yet; its clock has not started.
UPDATE task_attempts SET progress_at = $2 WHERE id = $1 AND progress_marker IS NOT NULL;

-- name: ListAttemptWorkspaceCleanups :many
-- Final attempts use a separate bounded batch, so cleanup cannot starve runtime stop resolution.
-- Unchecked cleanup comes first; unresolved looks move behind other due work durably.
SELECT * FROM task_attempts
WHERE state IN ('superseded', 'failed', 'cancelled', 'completed')
  AND workspace_request_id IS NOT NULL AND brief_delivery_id IS NULL AND NOT workspace_cleanup_confirmed
  AND updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
  AND (workspace_cleanup_after IS NULL OR workspace_cleanup_after <= now())
ORDER BY workspace_cleanup_after NULLS FIRST, updated_at, id
LIMIT $1;

-- name: SetTaskAttempt :one
UPDATE tasks
SET current_attempt_id = sqlc.narg(current_attempt_id), status = sqlc.arg(status), reason = sqlc.narg(reason),
    accepted_environment = coalesce(accepted_environment, sqlc.narg(accepted_environment)::jsonb),
    decision_attempt_id = CASE WHEN sqlc.arg(status) = 'blocked' THEN decision_attempt_id END,
    decision_report_id = CASE WHEN sqlc.arg(status) = 'blocked' THEN decision_report_id END,
    decision_action = CASE WHEN sqlc.arg(status) = 'blocked' THEN decision_action END,
    version = version + 1, updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- Capacity: attempts that still hold a slot, plus tasks not yet given an attempt.
-- name: CountParentCapacity :one
SELECT count(*) FROM tasks t
WHERE t.owner_id = $1 AND t.parent_task_id = $2
  AND (EXISTS (SELECT 1 FROM task_attempts a WHERE a.task_id = t.id AND a.capacity_held)
       OR (t.status NOT IN ('completed', 'failed', 'cancelled') AND NOT EXISTS (SELECT 1 FROM task_attempts a WHERE a.task_id = t.id)));

-- name: CountGlobalCapacity :one
SELECT count(*) FROM tasks t
WHERE t.owner_id = $1 AND t.parent_task_id IS NOT NULL
  AND (EXISTS (SELECT 1 FROM task_attempts a WHERE a.task_id = t.id AND a.capacity_held)
       OR (t.status NOT IN ('completed', 'failed', 'cancelled') AND NOT EXISTS (SELECT 1 FROM task_attempts a WHERE a.task_id = t.id)));

-- Writer claims. A task claim is keyed by its attempt and leaves binding_id empty.

-- name: ClaimTaskWriter :one
INSERT INTO workspace_writer_claims (owner_id, repository, branch, generation, attempt_id, held)
VALUES ($1, $2, $3, 1, $4, true)
ON CONFLICT (owner_id, repository, branch) DO UPDATE
SET generation = workspace_writer_claims.generation + 1, attempt_id = EXCLUDED.attempt_id, binding_id = NULL,
    held = true, fence_evidence_ref = NULL, fenced_at = NULL
WHERE NOT workspace_writer_claims.held
RETURNING generation;

-- name: GetTaskWriterClaim :one
SELECT * FROM workspace_writer_claims WHERE attempt_id = $1;

-- Releases the claim by compare-and-swap on the generation: a claim already moved on is left alone.
-- name: ReleaseTaskWriter :execrows
UPDATE workspace_writer_claims
SET held = false, fence_evidence_ref = $3, fenced_at = now()
WHERE attempt_id = $1 AND generation = $2 AND held;

-- Operations driven by the reconcile loop. Every write is fenced on the claim.

-- name: GetTaskOperationByID :one
SELECT * FROM task_operations WHERE id = $1;

-- name: AdvanceTaskOperation :one
UPDATE task_operations
SET state = sqlc.arg(state), last_confirmed_step = sqlc.arg(step), reason = sqlc.narg(reason),
    attempt_id = coalesce(sqlc.narg(attempt_id), attempt_id),
    target_attempt_id = coalesce(sqlc.narg(target_attempt_id), target_attempt_id),
    lease_owner = NULL, lease_until = NULL, next_attempt_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
RETURNING *;

-- name: SetTaskOperationSource :exec
UPDATE task_operations SET source_attempt_id = $2, updated_at = now() WHERE id = $1;

-- name: OpenOperationsForTask :one
SELECT count(*) FROM task_operations WHERE task_id = $1 AND state NOT IN ('completed', 'blocked');

-- Event deliveries.

-- name: GetTaskEventDelivery :one
SELECT * FROM task_event_deliveries WHERE id = $1;

-- name: CountOpenEventDeliveries :one
SELECT count(*) FROM task_event_deliveries d
JOIN task_reports r ON r.id = d.report_id
WHERE r.task_id = $1 AND d.state IN ('pending', 'queued');

-- name: GetTaskReport :one
SELECT * FROM task_reports WHERE id = $1;

-- The delivery's target and message id are written together with its first claim, so a replay
-- finds the message id the ledger may already hold.
-- name: PrepareTaskEventDelivery :one
UPDATE task_event_deliveries
SET target_attempt_id = sqlc.narg(target_attempt_id), delivery_id = sqlc.arg(delivery_id),
    sends = sends + 1, updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
  AND delivery_id IS NULL
RETURNING *;

-- A message the ledger gave up on is recorded again under a new id.
-- name: ResendTaskEventDelivery :one
UPDATE task_event_deliveries
SET delivery_id = sqlc.arg(delivery_id), state = 'pending', sends = sends + 1, updated_at = now(),
    lease_owner = NULL, lease_until = NULL
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
RETURNING *;

-- name: SetTaskEventDeliveryState :one
UPDATE task_event_deliveries
SET state = sqlc.arg(state), updated_at = now(), lease_owner = NULL, lease_until = NULL
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
RETURNING *;

-- Projection (GitHub observation of the task's pull request).

-- name: ListProjectionCandidates :many
-- Code tasks that are not final and have a branch to look at, stalest first.
SELECT t.*, p.full_name AS project_full_name
FROM tasks t
JOIN projects p ON p.id = t.project_id
WHERE t.mode = 'code' AND t.status NOT IN ('completed', 'failed', 'cancelled')
  AND t.current_attempt_id IS NOT NULL
  AND (t.projected_at IS NULL OR t.projected_at < now() - make_interval(secs => sqlc.arg(stale_seconds)::float8))
ORDER BY t.projected_at NULLS FIRST, t.id
LIMIT $1;

-- name: GetAttemptCreatedPR :one
-- The newest pull request the attempt itself opened through Mainloop. The attempt's binding id is
-- the session id recorded on the creation row, so a later attempt (a new binding) never matches an
-- earlier attempt's PR. Owner, project and head branch must also be the task's, as on main.
SELECT c.pr_number
FROM pr_creations c
JOIN task_attempts a ON a.binding_id = c.session_id
JOIN tasks t ON t.id = a.task_id
WHERE a.id = $1 AND c.state = 'created' AND c.pr_number > 0
  AND c.owner_id = t.owner_id AND c.project_id = t.project_id AND c.head = t.checkout_branch
ORDER BY c.created_at DESC, c.id DESC
LIMIT 1;

-- name: SetTaskProjection :one
UPDATE tasks
SET pr_number = $2, pr_url = $3, pr_head_sha = $4, pr_state = $5, ci_state = $6, ci_head_sha = $7,
    pr_repository = sqlc.narg(pr_repository), pr_description = sqlc.narg(pr_description),
    projected_at = sqlc.narg(observed_at), version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- Refreshes the observation time without a new version or event.
-- name: TouchTaskProjection :exec
UPDATE tasks SET projected_at = sqlc.narg(observed_at), pr_description = sqlc.narg(pr_description) WHERE id = $1;

-- name: ReserveAttemptWorkspaceCreate :one
-- The caller holds the task lock, so a drain cannot race the reservation. A stale claim cannot
-- start another call. No transaction stays open across the workspace service call.
UPDATE task_attempts a
SET workspace_creates_inflight = workspace_creates_inflight + 1, updated_at = now()
WHERE a.id = sqlc.arg(id) AND a.state = 'creating' AND a.workspace_request_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM task_operations o WHERE o.id = sqlc.arg(operation_id)
              AND o.target_attempt_id = a.id AND o.state = 'target_creating'
              AND o.lease_owner = sqlc.arg(lease_owner) AND o.attempts = sqlc.arg(attempts))
RETURNING a.*;

-- name: FinishAttemptWorkspaceCreate :exec
UPDATE task_attempts
SET workspace_creates_inflight = workspace_creates_inflight - 1,
    workspace_cleanup_after = NULL, updated_at = now()
WHERE id = $1 AND workspace_creates_inflight > 0;

-- name: DeferAttemptWorkspaceCleanup :exec
-- Every unresolved look, including an observation/delete error, advances the retry schedule.
-- Five seconds doubles to a five-minute cap; elapsed time never releases a create reservation.
UPDATE task_attempts
SET workspace_cleanup_attempts = workspace_cleanup_attempts + 1,
    workspace_cleanup_after = now() + make_interval(
        secs => least(300, 5 * power(2, least(workspace_cleanup_attempts, 6)))::float8)
WHERE id = $1 AND state NOT IN ('creating', 'active') AND workspace_request_id IS NOT NULL
  AND brief_delivery_id IS NULL AND NOT workspace_cleanup_confirmed;

-- name: ConfirmAttemptWorkspaceCleanup :exec
-- Absence is conclusive only after the attempt stopped accepting creates and every reserved call
-- returned. Ending the task's create operation alone does not rule out a late commit.
UPDATE task_attempts a SET workspace_cleanup_confirmed = true, updated_at = now()
WHERE a.id = $1 AND a.state NOT IN ('creating', 'active') AND a.workspace_request_id IS NOT NULL
  AND a.workspace_creates_inflight = 0 AND a.brief_delivery_id IS NULL
  AND NOT a.workspace_cleanup_confirmed
  AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.request_id = a.workspace_request_id);

-- name: SetBindingRole :one
UPDATE agent_bindings SET role = $2, updated_at = now() WHERE session_id = $1 RETURNING *;

-- Internal lookups for the reconcile handlers, which know a row id and not the caller.

-- name: GetTaskByID :one
SELECT * FROM tasks WHERE id = $1;

-- name: GetTaskByIDForUpdate :one
SELECT * FROM tasks WHERE id = $1 FOR UPDATE;

-- Child tasks in id order, locked. Settling a supervisor takes its task, then these, then its
-- attempt: tasks before attempts everywhere.
-- name: ListChildTasksForUpdate :many
SELECT * FROM tasks WHERE parent_task_id = $1 ORDER BY id FOR UPDATE;

-- name: SetAttemptWriterGeneration :exec
UPDATE task_attempts SET writer_generation = $2, updated_at = now() WHERE id = $1 AND writer_generation IS NULL;

-- name: AppendAttemptEvidence :exec
UPDATE task_attempts
SET evidence_refs = CASE WHEN cardinality(evidence_refs) >= 64 OR $2::text = ANY (evidence_refs)
                         THEN evidence_refs ELSE array_append(evidence_refs, $2::text) END,
    updated_at = now()
WHERE id = $1;

-- A child's report also holds the parent's current attempt, so the parent cannot be drained
-- between the check and the commit. Returns no row when the parent has no current attempt.
-- name: LockParentCurrentAttemptShared :one
SELECT a.id, a.state, t.status AS task_status
FROM tasks t JOIN task_attempts a ON a.id = t.current_attempt_id
WHERE t.id = $1
FOR SHARE OF a;

-- name: GetBindingKagentSession :one
SELECT kagent_session_id FROM agent_bindings WHERE session_id = $1;

-- name: WorkspaceExists :one
SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1);

-- name: GetProjectFullName :one
SELECT full_name FROM projects WHERE id = $1;

-- name: ListOpenOperationsForSource :many
SELECT * FROM task_operations WHERE source_attempt_id = $1 AND state NOT IN ('completed', 'blocked');

-- name: GetWorkspaceEnvironment :one
SELECT environment FROM workspaces WHERE id = $1;

-- Coordination tasks whose supervisor reported completion and that wait for reconciliation.
-- name: ListReconcilingCoordinationTasks :many
SELECT id FROM tasks
WHERE mode = 'coordination' AND status = 'waiting' AND reason = 'reconciliation'
ORDER BY updated_at, id
LIMIT $1;

-- name: ListMergedPublicationTasks :many
-- Code tasks waiting for their publication whose own pull request is observed merged and that the
-- merge service settled: a merged candidate for that exact repository, PR number and head, proposed
-- by one of the task's own attempt sessions. A merge by anyone else is only observed. The newest
-- proposal is the one the task points at. With task_id it re-reads one task, under its lock.
SELECT DISTINCT ON (t.id) t.id, p.id AS proposal_id
FROM tasks t
JOIN task_attempts a ON a.task_id = t.id AND a.binding_id IS NOT NULL
JOIN merge_proposals p ON p.session_id = a.binding_id AND p.owner_id = t.owner_id
JOIN merge_candidates c ON c.id = p.candidate_id
WHERE t.mode = 'code' AND t.status = 'waiting' AND t.reason = 'publication'
  AND t.pr_state = 'merged' AND t.pr_number IS NOT NULL AND t.pr_head_sha IS NOT NULL
  AND c.state = 'merged' AND c.owner_id = t.owner_id AND c.project_id = t.project_id
  AND c.pr_number = t.pr_number AND c.head_sha = t.pr_head_sha AND lower(c.repository) = t.pr_repository
  AND (sqlc.narg(task_id)::text IS NULL OR t.id = sqlc.narg(task_id))
ORDER BY t.id, p.created_at DESC, p.id DESC
LIMIT $1;

-- name: CompleteTaskMerged :one
-- Completes a task the merge service merged. Only the completion sweep calls it.
UPDATE tasks
SET status = 'completed', reason = NULL, decision_attempt_id = NULL, decision_report_id = NULL,
    decision_action = NULL, merge_proposal_id = $3, version = version + 1, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING *;

-- name: DeferAttemptResolve :one
-- An unconfirmed stop that kagent still did not settle: count the look and wait before the next.
UPDATE task_attempts
SET resolve_attempts = resolve_attempts + 1, resolve_after = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING resolve_attempts;

-- name: ListFailedBriefAttempts :many
-- Active attempts whose latest brief the sessions ledger ended failed (a cancelled one is the owner
-- steering the child), and that have not been given up on yet: the brief watcher sends them again.
SELECT a.* FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
JOIN deliveries d ON d.id = a.brief_delivery_id
WHERE a.state = 'active' AND t.status NOT IN ('completed', 'failed', 'cancelled')
  AND d.state = 'failed'
  AND NOT ('brief-delivery-failed' = ANY (a.evidence_refs))
ORDER BY a.updated_at, a.id
LIMIT $1;

-- name: GetAttemptDeliveryOutcomeVersion :one
-- Read before native observation, outside a transaction. Once stop is requested, admission is
-- closed and the retained delivery outcomes only advance this total.
SELECT coalesce(sum(d.outcome_version), 0)::bigint AS outcome_version
FROM deliveries d
JOIN task_attempts a ON a.session_id = d.session_id
WHERE a.id = $1;

-- name: ConfirmAttemptStop :one
-- The observer saw nothing running for the attempt's session. This is the only way an attempt's
-- stop_state becomes confirmed; fence checks it. A send admitted before stop may still be in its
-- native call or have an unknown outcome. Those deliveries must reconcile before confirmation.
-- Any outcome committed after the observation began invalidates it, including delivered: an
-- accepted task may still be working. The next observation must see all native tasks final.
-- The version comparison and unresolved-delivery gate use the same UPDATE snapshot. No row means
-- the attempt is not waiting, a delivery has not resolved, or the observation is stale.
UPDATE task_attempts
SET stop_state = 'confirmed', updated_at = now()
WHERE task_attempts.id = sqlc.arg(id) AND task_attempts.state = 'draining'
  AND task_attempts.stop_state IN ('requested', 'unresolved')
  AND NOT EXISTS (
      SELECT 1 FROM deliveries d WHERE d.session_id = task_attempts.session_id
      AND d.state IN ('sending', 'uncertain')
  )
  AND (SELECT coalesce(sum(d.outcome_version), 0)::bigint FROM deliveries d
       WHERE d.session_id = task_attempts.session_id) = sqlc.arg(outcome_version)::bigint
RETURNING *;

-- name: MarkAttemptStopUnresolved :one
-- The resolver has looked the bound number of times. Returns no row when it was already marked, so
-- the caller raises the block once.
UPDATE task_attempts
SET stop_state = 'unresolved', updated_at = now()
WHERE id = $1 AND state = 'draining' AND stop_state = 'requested'
RETURNING *;

-- name: ListSessionNativeContexts :many
-- Every kagent session (A2A context) a Mainloop session has sent a message to, from all of its
-- deliveries.
SELECT DISTINCT kagent_session_id::text AS kagent_session_id FROM deliveries
WHERE session_id = $1 AND kagent_session_id IS NOT NULL AND kagent_session_id <> ''
ORDER BY kagent_session_id;

-- name: ListSessionPossiblyLandingDeliveries :many
-- Deliveries that were sent to kagent and that no task is recorded for, and that the ledger has not
-- resolved (still sending or uncertain) or that a cancel closed: the message may still land and start
-- a task, so the session counts as possibly running while they are inside the hold.
SELECT id, state, kagent_session_id, updated_at FROM deliveries
WHERE session_id = $1 AND kagent_session_id IS NOT NULL AND kagent_session_id <> ''
  AND (task_id IS NULL OR task_id = '')
  AND state IN ('sending', 'uncertain', 'cancelled')
ORDER BY created_at, id;
