-- name: InsertEvent :one
-- The outbox lock is taken inside the statement, before the identity value is drawn, so no caller
-- can insert out of commit order. Same key as db.XactLock(lock_name).
WITH lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock(hashtextextended(@lock_name::text, 0)) AS locked
)
INSERT INTO events (user_id, type, payload)
SELECT @user_id::text, @type::text, @payload::jsonb FROM lock
RETURNING id, created_at;

-- name: NotifyEvent :exec
SELECT pg_notify(@channel::text, @payload::text);

-- name: ListEventsAfter :many
SELECT id, user_id, type, payload, created_at FROM events WHERE id > $1 ORDER BY id LIMIT $2;

-- name: ListUserEventsAfter :many
SELECT id, user_id, type, payload, created_at FROM events
WHERE user_id = $1 AND id > $2 ORDER BY id LIMIT $3;

-- name: LatestEventID :one
SELECT greatest(
    coalesce((SELECT max(id) FROM events), 0),
    (SELECT pruned_through FROM event_floor)
)::bigint AS id;

-- name: EventFloor :one
SELECT pruned_through FROM event_floor;

-- name: DeleteEventsBefore :one
-- Deletes at most @batch of the oldest events created before @cutoff and reports how many and the
-- newest id deleted.
WITH doomed AS (
    SELECT e.id FROM events e WHERE e.created_at < @cutoff::timestamptz ORDER BY e.id LIMIT @batch::int
), deleted AS (
    DELETE FROM events WHERE id IN (SELECT id FROM doomed) RETURNING id
)
SELECT count(*)::int AS deleted, coalesce(max(id), 0)::bigint AS max_id FROM deleted;

-- name: RaiseEventFloor :exec
UPDATE event_floor SET pruned_through = greatest(pruned_through, $1);
