-- Task locks. The request key serialises operations with the same idempotency key; the task row
-- lock serialises changes to one task. Take them in that order.

-- name: LockTaskRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- Tasks

-- name: InsertTask :one
INSERT INTO tasks (
    id, owner_id, project_id, topic_id, parent_task_id, root_task_id, creator_binding_id, title, brief,
    mode, assigned_profile_id, selection_source, provider_constraint, checkout_branch, checkout_ref,
    checkout_depth, reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, 'provisioning_unavailable'
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

-- name: CountLiveChildTasks :one
SELECT count(*) FROM tasks
WHERE owner_id = $1 AND parent_task_id = $2 AND status NOT IN ('completed', 'failed', 'cancelled');

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
       coalesce(pt.status, '')::text AS parent_task_status, coalesce(pa.state, '')::text AS parent_attempt_state
FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
LEFT JOIN tasks pt ON pt.id = t.parent_task_id
LEFT JOIN task_attempts pa ON pa.id = pt.current_attempt_id
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

-- Provisioning and settlement. The attempt, the task's pointer to it and the writer claim change
-- in one transaction under the task row lock.

-- name: InsertTaskAttempt :one
INSERT INTO task_attempts (
    id, task_id, number, profile_id, native_provider, configuration_revision, agent_namespace, agent_name,
    role, depth, writer_generation, initial_ref, session_id, binding_id, workspace_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
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
    evidence_refs = CASE WHEN sqlc.narg(evidence_ref)::text IS NULL THEN evidence_refs
                         ELSE array_append(evidence_refs, sqlc.narg(evidence_ref)::text) END
WHERE id = sqlc.arg(id) AND state = sqlc.arg(from_state)
RETURNING *;

-- name: ListUnsettledAttempts :many
-- Attempts that still hold a runtime although their task has ended, and attempts stuck draining or
-- fenced: the settle sweep finishes them.
SELECT a.* FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
WHERE (a.state IN ('draining', 'fenced') OR (a.state IN ('creating', 'active') AND t.status IN ('completed', 'failed', 'cancelled')))
  AND a.updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
ORDER BY a.updated_at, a.id
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
    updated_at = now()
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
SET delivery_id = sqlc.arg(delivery_id), state = 'pending', sends = sends + 1, updated_at = now()
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

-- name: SetTaskProjection :one
UPDATE tasks
SET pr_number = $2, pr_url = $3, pr_head_sha = $4, pr_state = $5, ci_state = $6, ci_head_sha = $7,
    projected_at = now(), version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- Refreshes the observation time without a new version or event.
-- name: TouchTaskProjection :exec
UPDATE tasks SET projected_at = now() WHERE id = $1;

-- Workspaces made for an attempt whose create was interrupted before the attempt recorded them.
-- name: FindUnattachedWorkspace :one
SELECT w.id FROM workspaces w
WHERE w.user_id = $1 AND w.project_id = $2 AND w.branch = $3 AND w.created_at >= $4
  AND NOT EXISTS (SELECT 1 FROM task_attempts a WHERE a.workspace_id = w.id)
ORDER BY w.created_at, w.id
LIMIT 1;

-- name: SetBindingRole :one
UPDATE agent_bindings SET role = $2, updated_at = now() WHERE session_id = $1 RETURNING *;
