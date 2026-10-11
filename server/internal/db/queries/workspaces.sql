-- Workspaces and their lifecycle operations. Session and binding rows come from core.sql.

-- name: InsertWorkspace :one
INSERT INTO workspaces (
    id, user_id, project_id, repo_url, ref, branch, depth, agent_kind, agent_namespace, agent_name,
    ports, idle_timeout_minutes, environment, request_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetWorkspaceRow :one
-- One owned, unarchived workspace with the binding's Session id.
SELECT w.*, b.kagent_session_id, b.kagent_deleted_at
FROM workspaces w
JOIN sessions s ON s.id = w.id
JOIN agent_bindings b ON b.session_id = w.id
WHERE w.id = $1 AND w.user_id = $2 AND s.archived_at IS NULL;

-- name: GetWorkspaceForOp :one
-- A workspace by id for an operation handler: no owner or archive filter.
SELECT w.*, b.kagent_session_id, b.kagent_deleted_at
FROM workspaces w
JOIN agent_bindings b ON b.session_id = w.id
WHERE w.id = $1;

-- name: BumpWorkspace :one
UPDATE workspaces SET updated_at = now() WHERE id = $1 RETURNING *;

-- name: ListWorkspaceRows :many
SELECT w.*, b.kagent_session_id, b.kagent_deleted_at
FROM workspaces w
JOIN sessions s ON s.id = w.id
JOIN agent_bindings b ON b.session_id = w.id
WHERE w.user_id = $1 AND s.archived_at IS NULL
ORDER BY w.created_at DESC, w.id;

-- name: GetLatestCreateOp :one
SELECT * FROM workspace_ops
WHERE workspace_id = $1 AND kind = 'create'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListLatestCreateOps :many
SELECT DISTINCT ON (o.workspace_id) o.*
FROM workspace_ops o
JOIN workspaces w ON w.id = o.workspace_id
WHERE w.user_id = $1 AND o.kind = 'create'
ORDER BY o.workspace_id, o.created_at DESC, o.id DESC;

-- name: GetWorkspaceOp :one
SELECT * FROM workspace_ops WHERE id = $1;

-- name: GetOpenWorkspaceOp :one
SELECT * FROM workspace_ops WHERE workspace_id = $1 AND state IN ('pending', 'retrying');

-- name: InsertWorkspaceOp :one
INSERT INTO workspace_ops (id, workspace_id, kind, idempotency_key, quiet_since)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: FinishWorkspaceOp :execrows
-- Settles the claim's operation. The fence matches only the current claim of a still-open row, so a
-- cancelled operation or a claimer that lost its lease records nothing.
UPDATE workspace_ops
SET state = $4, last_error = $5, lease_owner = NULL, lease_until = NULL
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'retrying');

-- name: RetryWorkspaceOp :execrows
-- Records a transient failure and schedules the next attempt.
UPDATE workspace_ops
SET state = 'retrying', failures = failures + 1,
    first_failed_at = coalesce(first_failed_at, now()), last_error = $4,
    next_attempt_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8),
    lease_owner = NULL, lease_until = NULL
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'retrying');

-- name: CancelOpenWorkspaceOps :execrows
-- Supersedes the open operations other than a delete; the delete takes over.
UPDATE workspace_ops
SET state = 'cancelled', lease_owner = NULL, lease_until = NULL
WHERE workspace_id = $1 AND state IN ('pending', 'retrying') AND kind <> 'delete';

-- name: BindWorkspaceSession :execrows
-- Records the Session kagent confirmed. A binding that already names another Session keeps it.
UPDATE agent_bindings
SET kagent_session_id = $2, updated_at = now()
WHERE session_id = $1 AND (kagent_session_id IS NULL OR kagent_session_id = $2);

-- name: TouchWorkspace :execrows
UPDATE workspaces SET last_active_at = now(), updated_at = now() WHERE id = $1;

-- name: MarkWorkspaceIdleSuspended :exec
UPDATE workspaces SET idle_suspended_at = now(), updated_at = now() WHERE id = $1;

-- name: GetWorkspaceActivity :one
-- The newest owner activity the workspace itself recorded.
SELECT greatest(coalesce(last_active_at, created_at), created_at)::timestamptz AS activity
FROM workspaces WHERE id = $1;

-- name: ListIdleWorkspaceCandidates :many
-- Workspaces whose own activity is older than their idle timeout and that are not already parked
-- since that activity. Delivery activity is checked by the caller against the delivery ledger.
SELECT w.id, w.user_id, w.idle_timeout_minutes, b.kagent_session_id AS kagent_session_id,
       a.activity::timestamptz AS activity
FROM workspaces w
JOIN sessions s ON s.id = w.id
JOIN agent_bindings b ON b.session_id = w.id
CROSS JOIN LATERAL (
    SELECT greatest(coalesce(w.last_active_at, w.created_at), w.created_at) AS activity
) a
WHERE s.archived_at IS NULL
  AND b.kagent_session_id IS NOT NULL AND b.kagent_deleted_at IS NULL AND b.role <> 'main'
  AND a.activity < now() - make_interval(mins => w.idle_timeout_minutes)
  AND (w.idle_suspended_at IS NULL OR w.idle_suspended_at < a.activity)
  AND NOT EXISTS (
      SELECT 1 FROM workspace_ops o WHERE o.workspace_id = w.id AND o.state IN ('pending', 'retrying'))
ORDER BY a.activity, w.id
LIMIT $1;

-- name: ListArchivedUndeletedBindings :many
-- Archived sessions whose kagent Session has not been confirmed deleted.
SELECT b.session_id, b.kagent_session_id AS kagent_session_id
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE s.archived_at IS NOT NULL AND b.kagent_session_id IS NOT NULL AND b.kagent_deleted_at IS NULL
ORDER BY s.archived_at, b.session_id
LIMIT $1;

-- name: PruneWorkspaceOps :execrows
-- Finished operations other than the creates (which explain a failed workspace) are not kept.
DELETE FROM workspace_ops
WHERE kind <> 'create' AND state NOT IN ('pending', 'retrying')
  AND created_at < now() - make_interval(secs => sqlc.arg(keep_seconds)::float8);

-- name: ListBoundKagentSessionIDs :many
SELECT kagent_session_id AS kagent_session_id FROM agent_bindings WHERE kagent_session_id IS NOT NULL;

-- name: CountUnresolvedBindings :one
-- Bindings that may own a kagent Session Mainloop has not recorded: a create was sent (or may have
-- been) and no Session id came back. A workspace whose create was never sent cannot own one.
SELECT count(*) FROM agent_bindings b
WHERE b.kagent_session_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM workspaces w
      WHERE w.id = b.session_id
        AND NOT EXISTS (
            SELECT 1 FROM workspace_ops o
            WHERE o.workspace_id = w.id AND o.kind = 'create' AND o.attempts > 0));

-- name: GetWorkspaceEnvironmentVersion :one
-- The environment version a project's selection resolves to, when it is validated.
SELECT sel.environment_id, v.id AS version_id, v.registry, v.repository,
       v.platform_manifest_digest, v.architecture, v.validator_version
FROM project_environment_selections sel
JOIN dev_environments e ON e.id = sel.environment_id
JOIN environment_versions v
  ON v.environment_id = sel.environment_id AND v.id = coalesce(sel.version_id, e.default_version_id)
WHERE sel.project_id = $1 AND v.validation_status = 'static_validated';

-- name: WorkspaceCreateAttempted :one
-- Whether a create was ever sent: kagent may then own a Session Mainloop has not recorded.
SELECT EXISTS (
    SELECT 1 FROM workspace_ops WHERE workspace_id = $1 AND kind = 'create' AND attempts > 0);

-- name: ListWorkspaceAgents :many
-- Every Agent a workspace was created on, which may no longer be the configured one.
SELECT DISTINCT agent_namespace, agent_name FROM workspaces;
