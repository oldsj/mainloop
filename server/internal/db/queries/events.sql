-- name: InsertEvent :one
INSERT INTO events (user_id, type, payload) VALUES ($1, $2, $3) RETURNING id, created_at;

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

-- name: DeleteEventsBefore :many
DELETE FROM events WHERE created_at < $1 RETURNING id;

-- name: RaiseEventFloor :exec
UPDATE event_floor SET pruned_through = greatest(pruned_through, $1);
