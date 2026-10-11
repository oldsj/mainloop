-- Preview proxy: the target a preview host resolves to, and keep-awake leases.

-- name: GetPreviewTarget :one
-- The owner's unarchived workspace with a live Session: the only thing a preview may reach. A
-- workspace that belongs to a task attempt is reachable only while that attempt is active and the
-- task's current one; a fenced, superseded or finished attempt no longer owns it. This is a read,
-- not a lock: the dial checks again.
SELECT w.id, w.ports, b.kagent_session_id::text AS kagent_session_id
FROM workspaces w
JOIN sessions s ON s.id = w.id
JOIN agent_bindings b ON b.session_id = w.id
WHERE w.id = $1 AND w.user_id = $2 AND s.archived_at IS NULL
  AND b.kagent_session_id IS NOT NULL AND b.kagent_deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM task_attempts a JOIN tasks t ON t.id = a.task_id
    WHERE (a.session_id = w.id OR a.binding_id = w.id OR a.workspace_id = w.id)
      AND NOT (a.state = 'active' AND coalesce(t.current_attempt_id = a.id, false))
  );

-- name: UpsertPreviewLease :exec
-- Opens a lease, or renews it (also re-creating one a stalled holder lost to the prune).
INSERT INTO preview_leases (id, workspace_id, holder, expires_at)
VALUES ($1, $2, $3, now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (id) DO UPDATE SET expires_at = excluded.expires_at;

-- name: DeletePreviewLease :exec
DELETE FROM preview_leases WHERE id = $1;

-- name: ListLeasedWorkspaces :many
SELECT DISTINCT workspace_id FROM preview_leases WHERE expires_at > now();

-- name: PruneExpiredPreviewLeases :execrows
DELETE FROM preview_leases WHERE expires_at < now() - make_interval(secs => sqlc.arg(keep_seconds)::float8);
