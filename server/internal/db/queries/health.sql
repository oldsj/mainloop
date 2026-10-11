-- Orchestration health: the collector's writes, the retention sweep, and the read-only page.
-- Every read of the page is a plain SELECT; the health package runs them in a read-only transaction.

-- Who an observation belongs to. One row per session; the attempt (and so the task and project) is
-- whatever the session runs now. A session of no task belongs to its own project.
-- name: GetHealthAttribution :one
SELECT s.id AS session_id, b.kind AS provider,
       s.project_id AS session_project_id, t.project_id AS task_project_id,
       t.id AS task_id, a.id AS attempt_id, a.configuration_revision AS profile_revision
FROM sessions AS s
JOIN agent_bindings AS b ON b.session_id = s.id
LEFT JOIN task_attempts AS a ON a.session_id = s.id
LEFT JOIN tasks AS t ON t.id = a.task_id
WHERE s.id = $1 AND s.user_id = $2;

-- Locks the turns a receipt touches, in key order, so an ingest and a retention pass cannot
-- interleave on one turn. A frozen turn takes no more events.
-- name: LockHealthTurns :many
SELECT turn_key, frozen
FROM health_turns
WHERE owner_id = $1 AND gateway = $2 AND session_id = $3 AND a2a_task_id = $4 AND turn_key = ANY(@turn_keys::text[])
ORDER BY turn_key
FOR UPDATE;

-- name: GetHealthEvents :many
SELECT event_id, session_id, a2a_task_id, payload
FROM health_events
WHERE owner_id = $1 AND gateway = $2 AND event_id = ANY(@event_ids::text[]);

-- The events one receipt holds: the per-receipt bound.
-- name: CountHealthReceiptEvents :one
SELECT count(*) FROM health_events
WHERE owner_id = $1 AND gateway = $2 AND session_id = $3 AND a2a_task_id = $4;

-- name: InsertHealthEvent :execrows
INSERT INTO health_events (
    owner_id, gateway, event_id, producer_epoch, sequence, session_id, runtime_session_id, a2a_task_id,
    project_id, task_id, attempt_id, profile_revision, turn_key, occurred_at, payload
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT DO NOTHING;

-- name: ListHealthTurnEvents :many
SELECT sequence, occurred_at, payload
FROM health_events
WHERE owner_id = $1 AND gateway = $2 AND session_id = $3 AND a2a_task_id = $4 AND turn_key = $5
ORDER BY sequence;

-- A rebuilt turn. A frozen turn keeps its summary.
-- name: UpsertHealthTurn :exec
INSERT INTO health_turns (
    owner_id, gateway, session_id, a2a_task_id, turn_key, runtime_session_id, project_id, task_id, attempt_id,
    profile_revision, provider, occurred_at, summary, aggregation_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (owner_id, gateway, session_id, a2a_task_id, turn_key) DO UPDATE SET
    summary = excluded.summary,
    aggregation_version = excluded.aggregation_version,
    occurred_at = excluded.occurred_at
WHERE NOT health_turns.frozen;

-- The receipt of a task: written by the collector when it stores the task's events. A read of a
-- finished turn (@open false) makes the receipt collected for good. A read of a turn still running
-- leaves it pending, due again at @next_attempt_at, with its failure count cleared; it never takes
-- a collected receipt back.
-- name: UpsertHealthReceipt :exec
INSERT INTO health_collection (
    owner_id, gateway, session_id, a2a_task_id, delivery_id, receipt_at, project_id, state, reasons, attempts,
    next_attempt_at, collected_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, CASE WHEN @open::bool THEN 'pending' ELSE 'collected' END, @reasons::text[], 0,
    @next_attempt_at::timestamptz, CASE WHEN @open::bool THEN NULL ELSE now() END)
ON CONFLICT (owner_id, gateway, session_id, a2a_task_id) DO UPDATE SET
    state = CASE WHEN NOT @open::bool OR health_collection.state = 'collected' THEN 'collected' ELSE 'pending' END,
    reasons = CASE WHEN @open::bool AND health_collection.state = 'collected' THEN health_collection.reasons ELSE excluded.reasons END,
    project_id = excluded.project_id,
    attempts = CASE WHEN health_collection.state = 'collected' THEN health_collection.attempts ELSE 0 END,
    next_attempt_at = CASE WHEN health_collection.state = 'collected' OR NOT @open::bool
        THEN health_collection.next_attempt_at ELSE excluded.next_attempt_at END,
    collected_at = CASE WHEN @open::bool THEN health_collection.collected_at ELSE now() END,
    updated_at = now();

-- Deliveries that carry a kagent task, finished or still running, become receipts to collect. A
-- receipt is written once. Deliveries that already have one are excluded before the LIMIT, so a
-- window holding more than one batch of them never hides the newer ones; the walk follows the
-- deliveries_health index in created_at order and sorts nothing.
-- name: DiscoverHealthReceipts :execrows
WITH batch AS (
    SELECT d.id, d.session_id, d.task_id, d.created_at
    FROM deliveries AS d
    WHERE d.state IN ('delivered', 'completed', 'failed', 'cancelled')
      AND d.task_id IS NOT NULL AND d.kagent_session_id IS NOT NULL
      AND d.created_at > @since::timestamptz
      AND EXISTS (SELECT 1 FROM sessions AS o WHERE o.id = d.session_id AND o.user_id = @owner_id::text)
      AND NOT EXISTS (
          SELECT 1 FROM health_collection AS c
          WHERE c.owner_id = @owner_id::text AND c.gateway = @gateway::text AND c.session_id = d.session_id
            AND c.a2a_task_id = d.task_id)
    ORDER BY d.created_at, d.id
    LIMIT @max_rows::int
)
INSERT INTO health_collection (owner_id, gateway, session_id, a2a_task_id, delivery_id, receipt_at, project_id)
SELECT s.user_id, @gateway::text, b.session_id, b.task_id, b.id, b.created_at,
       (CASE WHEN a.id IS NULL THEN s.project_id ELSE t.project_id END)::text
FROM batch AS b
JOIN sessions AS s ON s.id = b.session_id
LEFT JOIN task_attempts AS a ON a.session_id = s.id
LEFT JOIN tasks AS t ON t.id = a.task_id
ON CONFLICT (owner_id, gateway, session_id, a2a_task_id) DO NOTHING;

-- The receipts due for a read, oldest first, with what is needed to find the Agent that owns the
-- task (as the HITL observer does).
-- name: ListDueHealthReceipts :many
SELECT c.session_id, c.a2a_task_id, c.delivery_id, c.receipt_at, c.attempts,
       (d.state IN ('completed', 'failed', 'cancelled'))::bool AS delivery_final,
       d.kagent_session_id AS runtime_session_id, b.kind, b.role,
       w.agent_namespace AS workspace_agent_namespace, w.agent_name AS workspace_agent_name
FROM health_collection AS c
JOIN deliveries AS d ON d.id = c.delivery_id
JOIN agent_bindings AS b ON b.session_id = c.session_id
LEFT JOIN workspaces AS w ON w.id = c.session_id
WHERE c.owner_id = $1 AND c.gateway = $2 AND c.state = 'pending'
  AND c.next_attempt_at <= @now::timestamptz AND c.receipt_at > @since::timestamptz
ORDER BY c.next_attempt_at, c.receipt_at
LIMIT @max_rows::int;

-- A read that failed: the receipt stays pending with the reason and a later retry, or becomes
-- unavailable once it has had its attempts.
-- name: FailHealthReceipt :exec
UPDATE health_collection
SET attempts = attempts + 1,
    state = CASE WHEN attempts + 1 >= @max_attempts::int THEN 'unavailable' ELSE 'pending' END,
    reasons = ARRAY['source_unavailable']::text[],
    next_attempt_at = @next_attempt_at::timestamptz,
    updated_at = now()
WHERE owner_id = $1 AND gateway = $2 AND session_id = $3 AND a2a_task_id = $4 AND state = 'pending';

-- A receipt still pending after the event window is closed: whatever it could still store would
-- already be expired.
-- name: CloseStaleHealthReceipts :execrows
UPDATE health_collection
SET state = 'unavailable', reasons = ARRAY['source_unavailable']::text[], updated_at = now()
WHERE owner_id = $1 AND gateway = $2 AND state = 'pending' AND receipt_at <= @since::timestamptz;

-- A receipt whose session, binding or delivery is gone cannot be read again.
-- name: AbandonOrphanHealthReceipts :execrows
UPDATE health_collection AS c
SET state = 'unavailable', reasons = ARRAY['source_unavailable']::text[], updated_at = now()
WHERE c.owner_id = $1 AND c.gateway = $2 AND c.state = 'pending'
  AND NOT EXISTS (
      SELECT 1 FROM deliveries AS d
      JOIN sessions AS s ON s.id = d.session_id
      JOIN agent_bindings AS b ON b.session_id = s.id
      WHERE d.id = c.delivery_id AND s.id = c.session_id);

-- Retention. A turn with an expired event is frozen first, so deleting the event cannot change its
-- totals.
-- name: FreezeHealthTurns :execrows
UPDATE health_turns AS t SET frozen = true
WHERE t.owner_id = $1 AND NOT t.frozen
  AND EXISTS (
      SELECT 1 FROM health_events AS e
      WHERE e.owner_id = t.owner_id AND e.gateway = t.gateway AND e.session_id = t.session_id
        AND e.a2a_task_id = t.a2a_task_id AND e.turn_key = t.turn_key AND e.occurred_at <= @cutoff::timestamptz);

-- Only the events of frozen turns go: an event that reached its turn after the freeze pass stays
-- for the next pass, so a summary never loses an event it counted.
-- name: DeleteExpiredHealthEvents :execrows
DELETE FROM health_events AS e
WHERE e.owner_id = $1 AND e.occurred_at <= @cutoff::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM health_turns AS t
      WHERE t.owner_id = e.owner_id AND t.gateway = e.gateway AND t.session_id = e.session_id
        AND t.a2a_task_id = e.a2a_task_id AND t.turn_key = e.turn_key AND NOT t.frozen);

-- name: DeleteExpiredHealthTurns :execrows
DELETE FROM health_turns WHERE owner_id = $1 AND occurred_at <= @cutoff::timestamptz;

-- name: DeleteExpiredHealthReceipts :execrows
DELETE FROM health_collection WHERE owner_id = $1 AND updated_at <= @cutoff::timestamptz;

-- The page. Ownership first: a record that is not the owner's answers 404, like a missing one.
-- name: HealthOwnsProject :one
SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND user_id = $2);

-- name: HealthOwnsTask :one
SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND owner_id = $2);

-- A session is the owner's while it exists, and afterwards as long as health rows remember it.
-- name: HealthOwnsSession :one
SELECT (EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND user_id = $2)
    OR EXISTS (SELECT 1 FROM health_turns WHERE session_id = $1 AND owner_id = $2))::bool;

-- name: HealthOwnsAttempt :one
SELECT EXISTS (
    SELECT 1 FROM task_attempts AS a JOIN tasks AS t ON t.id = a.task_id WHERE a.id = $1 AND t.owner_id = $2);

-- name: ListHealthTurns :many
SELECT owner_id, gateway, session_id, a2a_task_id, turn_key, runtime_session_id, project_id, task_id, attempt_id,
       profile_revision, provider, occurred_at, summary, aggregation_version, frozen
FROM health_turns
WHERE owner_id = @owner_id::text AND occurred_at > @since::timestamptz
  AND (sqlc.narg('project_id')::text IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('task_id')::text IS NULL OR task_id = sqlc.narg('task_id'))
  AND (sqlc.narg('session_id')::text IS NULL OR session_id = sqlc.narg('session_id'))
  AND (sqlc.narg('attempt_id')::text IS NULL OR attempt_id = sqlc.narg('attempt_id'))
  AND (sqlc.narg('provider')::text IS NULL OR provider = sqlc.narg('provider'))
ORDER BY occurred_at DESC, session_id, a2a_task_id, turn_key
LIMIT 2001;

-- The owner's sessions the page should list, with their task: those made or active inside the
-- window, plus the ones the turns belong to.
-- name: ListHealthSessions :many
SELECT s.id, s.title, s.created_at, b.kind AS provider,
       s.project_id AS session_project_id, t.project_id AS task_project_id,
       a.id AS attempt_id, t.id AS task_id, t.title AS task_title
FROM sessions AS s
JOIN agent_bindings AS b ON b.session_id = s.id
LEFT JOIN task_attempts AS a ON a.session_id = s.id
LEFT JOIN tasks AS t ON t.id = a.task_id
WHERE s.user_id = @owner_id::text
  AND (s.created_at > @since::timestamptz
       OR EXISTS (SELECT 1 FROM deliveries AS d WHERE d.session_id = s.id AND d.created_at > @since::timestamptz)
       OR s.id = ANY(@session_ids::text[]))
  AND (sqlc.narg('project_id')::text IS NULL
       OR CASE WHEN a.id IS NULL THEN s.project_id ELSE t.project_id END = sqlc.narg('project_id'))
  AND (sqlc.narg('task_id')::text IS NULL OR t.id = sqlc.narg('task_id'))
  AND (sqlc.narg('session_id')::text IS NULL OR s.id = sqlc.narg('session_id'))
  AND (sqlc.narg('attempt_id')::text IS NULL OR a.id = sqlc.narg('attempt_id'))
  AND (sqlc.narg('provider')::text IS NULL OR b.kind = sqlc.narg('provider'))
ORDER BY s.created_at DESC, s.id
LIMIT 501;

-- Tasks no session runs: they appear with no session, so work that never started is visible.
-- name: ListHealthUnassignedTasks :many
SELECT t.id, t.project_id, t.title
FROM tasks AS t
WHERE t.owner_id = @owner_id::text AND t.created_at > @since::timestamptz
  AND NOT EXISTS (SELECT 1 FROM task_attempts AS a WHERE a.task_id = t.id AND a.session_id IS NOT NULL)
  AND (sqlc.narg('project_id')::text IS NULL OR t.project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('task_id')::text IS NULL OR t.id = sqlc.narg('task_id'))
ORDER BY t.created_at DESC, t.id
LIMIT 501;

-- name: ListHealthReports :many
SELECT id, task_id, attempt_id, outcome
FROM task_reports
WHERE task_id = ANY(@task_ids::text[]) AND outcome IN ('blocked', 'failed') AND created_at > @since::timestamptz
ORDER BY created_at, id;

-- name: ListHealthApprovals :many
SELECT id, session_id, kagent_session_id, availability
FROM hitl_requests
WHERE owner_id = $1 AND session_id = ANY(@session_ids::text[]) AND created_at > @since::timestamptz
ORDER BY created_at, id;

-- The receipts that belong to the window (their delivery or one of their turns is inside it) and
-- why some are not whole. Only these describe the window's coverage.
-- name: ListHealthReceipts :many
SELECT health_collection.session_id, health_collection.a2a_task_id, health_collection.state, health_collection.reasons
FROM health_collection
WHERE health_collection.owner_id = $1 AND health_collection.session_id = ANY(@session_ids::text[])
  AND (health_collection.receipt_at > @since::timestamptz
       OR EXISTS (
           SELECT 1 FROM health_turns AS t
           WHERE t.owner_id = health_collection.owner_id AND t.gateway = health_collection.gateway
             AND t.session_id = health_collection.session_id AND t.a2a_task_id = health_collection.a2a_task_id
             AND t.occurred_at > @since::timestamptz));

-- Turns that should have left a receipt: the finished deliveries discovery tracks.
-- name: ListHealthExpectedReceipts :many
SELECT session_id, count(*)::int AS expected
FROM deliveries
WHERE session_id = ANY(@session_ids::text[]) AND task_id IS NOT NULL AND kagent_session_id IS NOT NULL
  AND state IN ('completed', 'failed', 'cancelled') AND created_at > @since::timestamptz
GROUP BY session_id;

-- name: ListHealthEventRows :many
SELECT event_id, task_id, attempt_id, received_at, payload
FROM health_events
WHERE owner_id = $1 AND session_id = $2 AND occurred_at > @since::timestamptz
ORDER BY occurred_at, sequence, event_id
LIMIT 201;

-- The operator smoke run's ledger facts. Grants stay private.
-- name: ListSmokeCapacityHolders :many
SELECT DISTINCT t.id
FROM task_attempts AS a
JOIN tasks AS t ON t.id = a.task_id
WHERE a.capacity_held AND t.owner_id = $1 AND t.parent_task_id IS NULL
ORDER BY t.id;

-- name: CountSmokeHeldAttempts :one
SELECT count(*) FROM task_attempts WHERE capacity_held;

-- name: ListSmokeDeliveries :many
SELECT d.id AS message_id, d.state
FROM deliveries AS d
JOIN sessions AS s ON s.id = d.session_id
WHERE s.user_id = $1
  AND (s.project_id = $2 OR EXISTS (
      SELECT 1 FROM agent_bindings AS b WHERE b.session_id = s.id AND b.role = 'main'))
  AND d.state IN ('recorded', 'sending', 'delivered', 'uncertain')
ORDER BY d.id
LIMIT 101;

-- Publications are keyed by repository; the project's full name is the join.
-- name: ListSmokePushes :many
SELECT g.request_id, g.state, g.branch
FROM git_publications AS g
JOIN projects AS p ON lower(p.full_name) = g.repository
WHERE p.id = $1 AND p.user_id = $2 AND g.branch = $3
ORDER BY g.updated_at DESC, g.request_id
LIMIT 101;

-- name: SmokePushConfirmed :one
SELECT EXISTS (
    SELECT 1 FROM git_publications AS g
    JOIN projects AS p ON lower(p.full_name) = g.repository
    WHERE p.id = $1 AND p.user_id = $2 AND g.branch = $3 AND g.state = 'confirmed');
