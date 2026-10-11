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

-- The task principal behind an MCP binding: its attempt, the task it belongs to and whether that
-- attempt is still the task's current one.
-- name: GetAttemptPrincipal :one
SELECT a.id AS attempt_id, a.task_id, a.state AS attempt_state, a.role AS attempt_role, a.depth AS attempt_depth,
       t.owner_id, t.status AS task_status, t.mode AS task_mode, t.project_id, t.root_task_id, t.parent_task_id,
       (coalesce(t.current_attempt_id = a.id, false))::boolean AS is_current
FROM task_attempts a
JOIN tasks t ON t.id = a.task_id
WHERE a.binding_id = $1;

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
INSERT INTO task_event_deliveries (report_id, recipient_key) VALUES ($1, $2);

-- name: ListTaskEventDeliveries :many
SELECT * FROM task_event_deliveries WHERE report_id = $1 ORDER BY recipient_key;

-- A change that leaves the status alone but that clients should reread: a new operation or report.
-- name: TouchTask :one
UPDATE tasks SET version = version + 1, updated_at = now() WHERE id = $1 AND version = $2 RETURNING *;

-- A task cancelled before it was provisioned ends its create request with it, so the reconcile
-- loop does not provision a cancelled task.
-- name: BlockRequestedCreateOps :exec
UPDATE task_operations SET state = 'blocked', updated_at = now()
WHERE task_id = $1 AND kind = 'create' AND state = 'requested';
