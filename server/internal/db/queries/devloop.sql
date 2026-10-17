-- name: LockDevloopLease :one
SELECT * FROM devloop_lease WHERE id = $1 FOR UPDATE;

-- name: GetDevloopLease :one
SELECT * FROM devloop_lease WHERE id = $1;

-- name: UpsertDevloopLeaseQueueEntry :exec
INSERT INTO devloop_lease_queue (lease_id, session_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: NextDevloopLeaseQueueEntry :one
SELECT session_id FROM devloop_lease_queue WHERE lease_id = $1 ORDER BY requested_at, session_id LIMIT 1;

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
SELECT count(*) FROM devloop_lease_queue WHERE lease_id = $1;

-- name: InsertDevloopOperation :one
INSERT INTO devloop_operations (id, idempotency_key, kind, session_id, project_id, params)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO NOTHING
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
UPDATE devloop_operations SET state = 'unknown', detail = $5,
    next_attempt_at = now() + make_interval(secs => $4::float8), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;
