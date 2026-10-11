-- Main thread, message mirror and delivery ledger (internal/sessions).
--
-- The "fenced" queries record a claimer's outcome: they match only the row's current claim
-- (id, lease_owner, attempts) and report rows affected, so a claimer that lost its lease changes
-- nothing. The ones that move to a final state also release the lease.

-- name: GetMainBinding :one
SELECT b.*, s.user_id, s.title, s.status AS session_status
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE b.role = 'main' AND s.user_id = $1 AND s.archived_at IS NULL
ORDER BY b.created_at
LIMIT 1;

-- name: GetOwnedSessionBinding :one
SELECT b.*, s.user_id, s.title, s.status AS session_status
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE s.id = $1 AND s.user_id = $2;

-- name: GetTopicName :one
SELECT name FROM topics WHERE id = $1;

-- name: ListTopicLines :many
SELECT t.name, t.status_line,
       (SELECT count(*) FROM topic_records r
        WHERE r.topic_id = t.id AND r.kind = 'pending' AND r.status = 'open')::int AS pending
FROM topics t
WHERE t.user_id = $1
ORDER BY t.updated_at DESC, t.id;

-- name: InsertSessionMessage :execrows
INSERT INTO session_messages (id, session_id, role, content)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO NOTHING;

-- name: ListSessionMessages :many
SELECT * FROM session_messages WHERE session_id = $1 ORDER BY created_at, id;

-- name: GetConversationSummary :one
SELECT s.id, s.user_id, s.title, s.created_at,
       (SELECT count(*) FROM session_messages m WHERE m.session_id = s.id)::int AS message_count,
       coalesce((SELECT max(m.created_at) FROM session_messages m WHERE m.session_id = s.id), s.created_at)::timestamptz AS updated_at
FROM sessions s
WHERE s.id = $1 AND s.user_id = $2;

-- name: InsertDelivery :one
INSERT INTO deliveries (id, idempotency_key, session_id, source)
VALUES ($1, $1, $2, $3)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetDelivery :one
SELECT * FROM deliveries WHERE id = $1;

-- name: ListSessionDeliveries :many
SELECT * FROM deliveries WHERE session_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2;

-- A delivery that stops a new send: an open one, or an uncertain one still inside the hold.
-- name: GetBlockingDelivery :one
SELECT id, state FROM deliveries
WHERE session_id = sqlc.arg(session_id)
  AND (state IN ('recorded', 'sending', 'delivered')
       OR (state = 'uncertain' AND updated_at > now() - make_interval(secs => sqlc.arg(hold_seconds)::float8)))
ORDER BY created_at DESC
LIMIT 1;

-- What a handler needs about the delivery's session.
-- name: GetDeliveryContext :one
SELECT d.id, d.session_id, d.state, d.task_id, d.evidence_ref, d.created_at, d.updated_at,
       b.kind, b.kagent_session_id, b.kagent_request_id, s.user_id,
       (SELECT m.content FROM session_messages m WHERE m.id = d.id) AS content
FROM deliveries d
JOIN agent_bindings b ON b.session_id = d.session_id
JOIN sessions s ON s.id = d.session_id
WHERE d.id = $1;

-- name: SetBindingKagentSessionIfUnset :execrows
UPDATE agent_bindings
SET kagent_session_id = $2, updated_at = now()
WHERE session_id = $1 AND kagent_session_id IS NULL;

-- name: MarkDeliverySending :execrows
UPDATE deliveries
SET state = 'sending', detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'recorded';

-- name: MarkDeliveryDelivered :execrows
UPDATE deliveries
SET state = 'delivered', task_id = $4, evidence_ref = $5, detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('sending', 'uncertain', 'delivered');

-- name: MarkDeliveryUncertain :execrows
UPDATE deliveries
SET state = 'uncertain', detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('recorded', 'sending', 'delivered', 'uncertain');

-- name: FinishDelivery :execrows
UPDATE deliveries
SET state = $4, detail = $5, task_id = coalesce(sqlc.narg(task_id), task_id),
    evidence_ref = coalesce(sqlc.narg(evidence_ref), evidence_ref),
    lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;

-- name: RefreshSessionStatus :exec
UPDATE sessions
SET status = CASE WHEN EXISTS (
        SELECT 1 FROM deliveries d
        WHERE d.session_id = sessions.id AND d.state IN ('recorded', 'sending', 'delivered'))
    THEN 'active' ELSE 'idle' END,
    started_at = coalesce(started_at, now())
WHERE sessions.id = $1 AND sessions.status IN ('pending', 'active', 'idle');

-- name: BumpBindingTurns :exec
UPDATE agent_bindings SET turns = turns + 1, updated_at = now() WHERE session_id = $1;
