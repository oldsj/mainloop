-- name: LockDevloopLease :one
SELECT * FROM devloop_lease WHERE id = $1 FOR UPDATE;

-- name: GetDevloopLease :one
SELECT * FROM devloop_lease WHERE id = $1;

-- name: UpsertDevloopLeaseQueueEntry :exec
INSERT INTO devloop_lease_queue (lease_id, session_id) VALUES ($1, $2)
ON CONFLICT (lease_id, session_id) DO UPDATE SET last_seen_at = now();

-- name: DeleteStaleDevloopLeaseQueueEntries :exec
DELETE FROM devloop_lease_queue
WHERE lease_id = $1 AND last_seen_at < now() - make_interval(secs => sqlc.arg(stale_secs)::float8);

-- name: NextDevloopLeaseQueueEntry :one
SELECT session_id FROM devloop_lease_queue
WHERE lease_id = $1 AND last_seen_at >= now() - make_interval(secs => sqlc.arg(stale_secs)::float8)
ORDER BY requested_at, session_id LIMIT 1;

-- name: GrantDevloopLease :one
UPDATE devloop_lease SET holder_session_id = $2, acquired_at = now(), last_used_at = now(), updated_at = now()
WHERE id = $1
RETURNING acquired_at, last_used_at;

-- name: TouchDevloopLease :one
UPDATE devloop_lease SET last_used_at = now(), updated_at = now() WHERE id = $1 AND holder_session_id = $2
RETURNING last_used_at;

-- name: ReleaseDevloopLease :execrows
UPDATE devloop_lease SET holder_session_id = NULL, acquired_at = NULL, last_used_at = NULL, updated_at = now()
WHERE id = $1 AND holder_session_id = $2;

-- name: DequeueDevloopLeaseSession :exec
DELETE FROM devloop_lease_queue WHERE lease_id = $1 AND session_id = $2;

-- name: DevloopLeaseQueuePosition :one
SELECT count(*) FROM devloop_lease_queue AS q
WHERE q.lease_id = $1 AND (q.requested_at, q.session_id) < (
    SELECT s.requested_at, s.session_id FROM devloop_lease_queue AS s WHERE s.lease_id = $1 AND s.session_id = $2
);

-- name: DevloopLeaseQueueLength :one
SELECT count(*) FROM devloop_lease_queue
WHERE lease_id = $1 AND last_seen_at >= now() - make_interval(secs => sqlc.arg(stale_secs)::float8);

-- A session that ended gives up its place in line and its hold on the lease.
-- name: DropDevloopSessionQueueEntries :exec
DELETE FROM devloop_lease_queue WHERE session_id = $1;

-- name: DropDevloopSessionHold :execrows
UPDATE devloop_lease SET holder_session_id = NULL, acquired_at = NULL, last_used_at = NULL, updated_at = now()
WHERE holder_session_id = $1;

-- A repeated call collapses onto a row still in flight (no row comes back and the caller reads
-- the existing one), but a row that already finished is run again: the same commit can need a
-- second sync after a failure, a pod restart or another holder's sync in between.
-- name: InsertDevloopOperation :one
INSERT INTO devloop_operations (id, idempotency_key, kind, session_id, project_id, params)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO UPDATE SET
    state = 'pending', params = EXCLUDED.params, result = NULL, detail = NULL, attempts = 0,
    interrupted = 0, created_at = now(), next_attempt_at = now(), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE devloop_operations.state IN ('succeeded', 'failed')
RETURNING *;

-- name: GetDevloopOperation :one
SELECT * FROM devloop_operations WHERE id = $1;

-- name: GetDevloopOperationByIdempotencyKey :one
SELECT * FROM devloop_operations WHERE idempotency_key = $1;

-- name: FinishDevloopOperation :execrows
UPDATE devloop_operations SET state = $4, result = $5, detail = $6, lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;

-- name: RetryDevloopOperationAfter :execrows
UPDATE devloop_operations SET state = 'unknown', detail = $5, interrupted = interrupted + 1,
    next_attempt_at = now() + make_interval(secs => $4::float8), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;

-- A new sync from a session replaces its older ones that are still waiting. A row a replica is
-- working on right now (live lease_until) is left to finish; the sync lock keeps the new one
-- behind it.
-- name: SupersedeDevloopSyncs :execrows
UPDATE devloop_operations SET state = 'failed', detail = 'superseded by a newer sync from this session',
    lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE kind = 'sync' AND session_id = $1 AND id <> $2 AND state IN ('pending', 'unknown')
  AND (lease_until IS NULL OR lease_until < now());

-- name: NewerDevloopSyncExists :one
SELECT EXISTS (
    SELECT 1 FROM devloop_operations
    WHERE kind = 'sync' AND session_id = $1 AND id <> $2 AND created_at > $3
      AND state IN ('pending', 'unknown', 'succeeded')
);

-- name: TakeDevloopSyncLock :execrows
UPDATE devloop_sync_lock SET op_id = $2, held_until = now() + make_interval(secs => sqlc.arg(hold_secs)::float8),
    updated_at = now()
WHERE id = $1 AND (op_id IS NULL OR held_until < now() OR op_id = $2);

-- name: DropDevloopSyncLock :exec
UPDATE devloop_sync_lock SET op_id = NULL, held_until = NULL, updated_at = now() WHERE id = $1 AND op_id = $2;

-- The owner's view of dev_sync history, newest first. An empty session_id means every session.
-- name: ListDevloopSyncs :many
SELECT * FROM devloop_operations
WHERE kind = 'sync' AND (sqlc.arg(session_id)::text = '' OR session_id = sqlc.arg(session_id)::text)
ORDER BY created_at DESC, id
LIMIT 50;

-- A project's staging target: owner-only writes (through the project API), reads visible to that
-- project's tasks (the MCP handlers read it to decide whether devloop tools are available and
-- which lease and sync lock to use).
-- name: GetDevloopTargetByProject :one
SELECT * FROM devloop_targets WHERE project_id = $1;

-- name: GetDevloopTarget :one
SELECT * FROM devloop_targets WHERE id = $1;

-- Keeps the row's id (and so its lease and sync lock) stable across an update: id is only used on
-- first insert, never overwritten by a later upsert.
-- name: UpsertDevloopTarget :one
INSERT INTO devloop_targets (
    id, project_id, kind, namespace, vcluster_name, vcluster_namespace, vcluster_server, workload,
    command_container, sync_paths, restart_cmd, migrate_cmd, health_cmd, smoke_cmd
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (project_id) DO UPDATE SET
    kind = EXCLUDED.kind, namespace = EXCLUDED.namespace, vcluster_name = EXCLUDED.vcluster_name,
    vcluster_namespace = EXCLUDED.vcluster_namespace, vcluster_server = EXCLUDED.vcluster_server,
    workload = EXCLUDED.workload, command_container = EXCLUDED.command_container, sync_paths = EXCLUDED.sync_paths,
    restart_cmd = EXCLUDED.restart_cmd, migrate_cmd = EXCLUDED.migrate_cmd, health_cmd = EXCLUDED.health_cmd,
    smoke_cmd = EXCLUDED.smoke_cmd, updated_at = now()
RETURNING *;

-- Every target needs its own lease and sync lock row, created once alongside it; a later update to
-- the same target (same id) leaves an in-progress lease or sync untouched.
-- name: EnsureDevloopLease :exec
INSERT INTO devloop_lease (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;

-- name: EnsureDevloopSyncLock :exec
INSERT INTO devloop_sync_lock (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;
