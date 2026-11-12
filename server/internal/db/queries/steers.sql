-- The turn a steer would join: the session's open delivery, when it is running a task Mainloop
-- knows. A turn still being sent, or one parked on an uncertain send, has no task to join yet.
-- name: GetSteerableDelivery :one
SELECT id, task_id::text AS task_id FROM deliveries
WHERE session_id = sqlc.arg(session_id) AND state = 'delivered' AND task_id IS NOT NULL
LIMIT 1;

-- name: InsertSteer :one
INSERT INTO delivery_steers (id, idempotency_key, session_id, delivery_id, source)
VALUES (sqlc.arg(id), sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(delivery_id), sqlc.arg(source))
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetSteer :one
SELECT * FROM delivery_steers WHERE id = $1;

-- Every steer the turn holds that has not been sent: the turn is ending, so they become the
-- session's next turns (converted) or end with it (cancelled). The caller holds
-- mainloop:send:<session>; the lease is cleared, so a claimer that held one loses its fence.
-- name: EndUnsentSteers :many
UPDATE delivery_steers
SET state = sqlc.arg(state), detail = sqlc.narg(detail), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE delivery_id = sqlc.arg(delivery_id) AND state = 'recorded'
RETURNING id, session_id, source, created_at;

-- A converted steer's ordinary delivery: queued behind whatever holds the session's turn, in the
-- order the message was recorded, so the backlog promotion admits it before anything sent after it.
-- name: InsertConvertedSteerDelivery :execrows
INSERT INTO deliveries (id, idempotency_key, session_id, source, state, created_at)
VALUES (sqlc.arg(id), sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(source), 'queued', sqlc.arg(created_at))
ON CONFLICT (id) DO NOTHING;

-- name: MarkSteerSending :execrows
UPDATE delivery_steers
SET state = 'sending', detail = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
  AND state = 'recorded';

-- A send that kagent proved it did not take goes back to recorded, to be sent again.
-- name: MarkSteerUnsent :execrows
UPDATE delivery_steers
SET state = 'recorded', detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
  AND state = 'sending';

-- name: SetSteerState :execrows
UPDATE delivery_steers
SET state = sqlc.arg(state), detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts);

-- Ends a steer in a final state, releasing its lease.
-- name: FinishSteer :execrows
UPDATE delivery_steers
SET state = sqlc.arg(state), detail = sqlc.narg(detail), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts);

-- The open steers of a session, oldest first, for the session's live state.
-- name: ListSessionSteers :many
SELECT id, delivery_id, state, detail, created_at, updated_at FROM delivery_steers
WHERE session_id = sqlc.arg(session_id)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(limit_count);
