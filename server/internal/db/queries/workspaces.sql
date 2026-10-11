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

-- name: GetWorkspaceByRequestID :one
-- An owned workspace by the request id its create was made with, archived or not.
SELECT w.*, b.kagent_session_id, b.kagent_deleted_at
FROM workspaces w
JOIN agent_bindings b ON b.session_id = w.id
WHERE w.request_id = $1 AND w.user_id = $2;

-- name: FreezeWorkspaceGitRead :one
-- Records the create's git-read decision once: a NULL credential freezes "no git read". It matches
-- only a workspace not yet decided, so the first decision stands and a later caller reads it back.
UPDATE workspaces SET git_read_decided = true, git_read_credential = $2
WHERE id = $1 AND NOT git_read_decided
RETURNING git_read_decided, git_read_credential;

-- name: FreezeWorkspaceGitPush :one
UPDATE workspaces SET git_push_decided = true, git_push_credential = $2
WHERE id = $1 AND NOT git_push_decided
RETURNING git_push_credential;

-- name: GetWorkspacePushAttempt :one
-- T1's request identity locates the attempt before the caller attaches the returned workspace.
-- Standalone workspaces, stale attempts and lost writers get no reference. The project default is
-- returned, not filtered on: the caller refuses a default-branch workspace and waits while the
-- default is still unknown.
SELECT a.id AS attempt_id, p.default_branch
FROM workspaces w
JOIN sessions s ON s.id = w.id AND s.archived_at IS NULL
    AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
JOIN task_attempts a ON a.workspace_request_id = w.request_id AND a.state IN ('creating', 'active')
JOIN tasks t ON t.id = a.task_id AND t.current_attempt_id = a.id AND t.mode = 'code'
    AND t.owner_id = w.user_id AND t.project_id = w.project_id AND t.checkout_branch = w.branch
    AND t.status NOT IN ('completed', 'failed', 'cancelled')
JOIN projects p ON p.id = w.project_id AND p.user_id = w.user_id
JOIN workspace_writer_claims c ON c.attempt_id = a.id AND c.held
    AND c.owner_id = w.user_id AND c.repository = lower(p.full_name) AND c.branch = w.branch
    AND c.generation = a.writer_generation
WHERE w.id = $1
    AND (t.parent_task_id IS NULL OR EXISTS (
        SELECT 1 FROM tasks parent JOIN task_attempts pa ON pa.id = parent.current_attempt_id
        WHERE parent.id = t.parent_task_id AND parent.owner_id = t.owner_id
            AND parent.status NOT IN ('completed', 'failed', 'cancelled') AND pa.state = 'active'))
FOR SHARE OF s, a, t, p, c;

-- name: InsertWorkspacePushReservation :exec
INSERT INTO workspace_push_reservations (id, idempotency_key, workspace_id, attempt_id, reference, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetWorkspacePushReservation :one
SELECT * FROM workspace_push_reservations WHERE workspace_id = $1;

-- name: PublishWorkspacePushReservation :execrows
UPDATE workspace_push_reservations SET state = 'published', lease_owner = NULL, lease_until = NULL
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'planned'
    AND expires_at > clock_timestamp();

-- name: WorkspacePushReservationLive :one
SELECT EXISTS (
    SELECT 1 FROM workspace_push_reservations r
    JOIN workspaces w ON w.id = r.workspace_id
    JOIN sessions s ON s.id = w.id AND s.archived_at IS NULL
        AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
    JOIN task_attempts a ON a.id = r.attempt_id AND a.state IN ('creating', 'active')
    JOIN tasks t ON t.id = a.task_id AND t.current_attempt_id = a.id
        AND t.status NOT IN ('completed', 'failed', 'cancelled')
    JOIN workspace_writer_claims c ON c.attempt_id = a.id AND c.held AND c.generation = a.writer_generation
        AND c.owner_id = w.user_id AND c.branch = w.branch
    WHERE r.workspace_id = $1 AND r.state <> 'revoked'
        AND (r.state = 'published' OR r.expires_at > clock_timestamp())
        AND NOT EXISTS (SELECT 1 FROM workspace_ops o WHERE o.workspace_id = w.id AND
            (o.kind = 'delete' OR (o.kind = 'create' AND o.state IN ('rejected', 'gave_up', 'cancelled'))))
);

-- name: RevokeWorkspacePushReservation :exec
UPDATE workspace_push_reservations SET state = 'revoked', lease_owner = NULL, lease_until = NULL
WHERE workspace_id = $1 AND state <> 'revoked';

-- name: ListUnsettledWorkspacePushReservations :many
-- Reservations still live, and revoked ones whose grant or push credential is not (a crash after
-- Enroll but before its fence); revoking again is idempotent.
SELECT r.workspace_id FROM workspace_push_reservations r
WHERE r.state <> 'revoked'
    OR EXISTS (SELECT 1 FROM git_push_grants g WHERE g.workspace_id = r.workspace_id AND g.state <> 'revoked')
    OR EXISTS (SELECT 1 FROM credentials c WHERE c.binding_id = r.workspace_id AND c.kind = 'git_push'
        AND c.state IN ('planned', 'published'))
ORDER BY r.id LIMIT $1;

-- name: RevokeWorkspacePushCredential :exec
UPDATE credentials SET state = 'revoked', lease_owner = NULL, lease_until = NULL,
    next_attempt_at = now(), revoked_at = now(), updated_at = now()
WHERE binding_id = $1 AND kind = 'git_push' AND state IN ('planned', 'published');

-- name: RevokeWorkspacePushGrant :exec
UPDATE git_push_grants SET state = 'revoked', revoked_at = now(), updated_at = now()
WHERE workspace_id = $1 AND state <> 'revoked';

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
-- Workspaces whose activity (their own, and their session's deliveries') is older than their idle
-- timeout and that are not already parked since that activity.
SELECT w.id, w.user_id, w.idle_timeout_minutes, b.kagent_session_id AS kagent_session_id,
       a.activity::timestamptz AS activity
FROM workspaces w
JOIN sessions s ON s.id = w.id
JOIN agent_bindings b ON b.session_id = w.id
CROSS JOIN LATERAL (
    SELECT greatest(coalesce(w.last_active_at, w.created_at), w.created_at,
                    (SELECT max(d.updated_at) FROM deliveries d WHERE d.session_id = w.id)) AS activity
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
SELECT kagent_session_id AS kagent_session_id FROM agent_bindings WHERE kagent_session_id IS NOT NULL
UNION
-- A main thread's retired native Sessions are kept, suspended, as history.
SELECT kagent_session_id FROM main_provider_history WHERE kagent_session_id IS NOT NULL;

-- name: CountUnresolvedBindings :one
-- Bindings that may own a kagent Session Mainloop has not recorded: a create was sent (or may have
-- been) and no Session id came back. A workspace whose create was never sent cannot own one. Any
-- other binding is counted from the moment its create was requested (the request id is written
-- before the call), until it is resolved; a session whose create was never requested (it was
-- cancelled before any send, say) is not counted.
SELECT count(*) FROM agent_bindings b
WHERE b.kagent_session_id IS NULL
  AND CASE WHEN EXISTS (SELECT 1 FROM workspaces w WHERE w.id = b.session_id)
      THEN EXISTS (
          SELECT 1 FROM workspace_ops o
          WHERE o.workspace_id = b.session_id AND o.kind = 'create' AND o.attempts > 0)
      ELSE b.kagent_create_request_id IS NOT NULL
      END;

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

-- name: ListKnownAgents :many
-- Every Agent a create was ever sent to, whether or not a row still names it: bindings' creates and
-- workspaces, recorded by triggers.
SELECT namespace, name FROM kagent_agents;

-- name: LockWorkspace :one
-- Locks the workspace row. An operation insert needs a share lock on it for its foreign key, so
-- it waits for the transaction that holds this.
SELECT id FROM workspaces WHERE id = $1 FOR UPDATE;

-- name: ListArchivedWorkspacesWithoutSession :many
-- Archived workspaces whose kagent Session id was never recorded although a create was sent, and
-- that have no operation open: the create reply may have been lost, so a Session may exist that
-- only a replay of the create can find. The delete operation does that and then purges the rows.
SELECT b.session_id
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
JOIN workspaces w ON w.id = b.session_id
WHERE s.archived_at IS NOT NULL AND b.kagent_session_id IS NULL
  AND EXISTS (
      SELECT 1 FROM workspace_ops o
      WHERE o.workspace_id = w.id AND o.kind = 'create' AND o.attempts > 0)
  AND NOT EXISTS (
      SELECT 1 FROM workspace_ops o WHERE o.workspace_id = w.id AND o.state IN ('pending', 'retrying'))
ORDER BY s.archived_at, b.session_id
LIMIT $1;

-- name: ForgetKnownAgent :execrows
-- Removes an Agent from the orphan sweep's set (the operator's remedy for an Agent that was
-- configured by mistake).
DELETE FROM kagent_agents WHERE namespace = $1 AND name = $2;

-- name: AgentInUse :one
-- Whether this deployment still has a workspace on the Agent, or a create sent to it that has not
-- been settled (no Session id recorded). Forgetting such an Agent would stop the orphan sweep from
-- reclaiming a Session that landed on it.
SELECT (EXISTS (SELECT 1 FROM workspaces WHERE agent_namespace = $1 AND agent_name = $2)
    OR EXISTS (
        SELECT 1 FROM agent_bindings
        WHERE kagent_create_agent_namespace = $1 AND kagent_create_agent_name = $2
          AND kagent_session_id IS NULL))::bool AS in_use;

-- name: CountPendingMainCreates :one
-- The unresolved bindings that are a main thread's switch target: its create was requested and no
-- Session id came back. A repeated PUT of the selection settles them.
SELECT count(*) FROM agent_bindings b
WHERE b.role = 'main' AND b.main_pending AND b.kagent_session_id IS NULL
  AND b.kagent_create_request_id IS NOT NULL;
