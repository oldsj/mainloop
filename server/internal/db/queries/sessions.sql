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
-- Prefer an adopted binding's stored Agent, otherwise the Agent its workspace was created on.
SELECT b.*, s.user_id, s.title, s.status AS session_status, s.archived_at,
       w.agent_namespace AS workspace_agent_namespace, w.agent_name AS workspace_agent_name,
       coalesce(b.kagent_agent_namespace, w.agent_namespace, '') AS resolved_agent_namespace,
       coalesce(b.kagent_agent_name, w.agent_name, '') AS resolved_agent_name
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
LEFT JOIN workspaces w ON w.id = b.session_id
WHERE s.id = $1 AND s.user_id = $2;

-- The attempt keeps its session id and non-none stop state after settlement. This is a permanent
-- send fence, independent of the session's activity status or the task's current attempt.
-- Standalone and adopted scheduled sessions have no task attempt and remain resumable.
-- Recording and the sending decision retain this lock until commit, before locking a delivery.
-- No row means this is a standalone session. No lock spans native I/O.
-- name: LockSessionTaskAttempt :one
SELECT stop_state FROM task_attempts WHERE session_id = $1 FOR SHARE;

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

-- What a handler needs about the delivery's session. The binding is a LEFT JOIN so a delivery
-- whose binding is gone still loads, and the handler can fail it instead of retrying for ever.
-- name: GetDeliveryContext :one
SELECT d.id, d.session_id, d.state, d.task_id, d.evidence_ref, d.created_at, d.updated_at,
       d.kagent_session_id AS sent_kagent_session_id,
       b.session_id AS binding_session_id, b.kagent_session_id, b.kagent_request_id,
       b.kind AS binding_kind, coalesce(b.role::text, '')::text AS binding_role,
       coalesce(b.mcp_grant_kind::text, '')::text AS binding_grant, s.user_id, s.title AS session_title,
       w.id AS workspace_id,
       coalesce(b.kagent_agent_namespace, w.agent_namespace, '') AS resolved_agent_namespace,
       coalesce(b.kagent_agent_name, w.agent_name, '') AS resolved_agent_name,
       (SELECT m.content FROM session_messages m WHERE m.id = d.id) AS content
FROM deliveries d
JOIN sessions s ON s.id = d.session_id
LEFT JOIN agent_bindings b ON b.session_id = d.session_id
LEFT JOIN workspaces w ON w.id = d.session_id
WHERE d.id = $1;

-- Stores the kagent Session the create returned. Fenced on the delivery's claim and on the
-- request id the create used, so a claimer that lost its lease, or one whose request id has since
-- been replaced, changes nothing.
-- name: SetDeliveryKagentSession :execrows
UPDATE agent_bindings b
SET kagent_session_id = sqlc.arg(kagent_session_id), updated_at = now()
FROM deliveries d
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND b.session_id = d.session_id
  AND b.kagent_session_id IS NULL
  AND b.kagent_request_id IS NOT DISTINCT FROM sqlc.narg(request_id)::text;

-- Drops a kagent Session that kagent no longer has, so the next create runs under a new request
-- id. Fenced on the delivery's claim and on the Session being replaced.
-- name: ReplaceDeliveryKagentSession :execrows
UPDATE agent_bindings b
SET kagent_session_id = NULL, kagent_request_id = sqlc.arg(new_request_id), standing_hash = NULL,
    kagent_create_request_id = NULL, kagent_create_agent_namespace = NULL, kagent_create_agent_name = NULL,
    updated_at = now()
FROM deliveries d
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND b.session_id = d.session_id
  AND b.kagent_session_id IS NOT DISTINCT FROM sqlc.narg(old_session_id)::text
  AND b.kagent_request_id IS NOT DISTINCT FROM sqlc.narg(old_request_id)::text;

-- Records, before the CreateSession call, that a create under this request id is about to be
-- sent, with the Agent it names. From then on the binding may own a kagent Session whose id
-- Mainloop has not stored, which the orphan sweep must not mistake for an orphan. Fenced like
-- SetDeliveryKagentSession.
-- name: MarkCreateRequested :execrows
UPDATE agent_bindings b
SET kagent_create_request_id = sqlc.arg(create_request_id),
    kagent_create_agent_namespace = sqlc.arg(agent_namespace), kagent_create_agent_name = sqlc.arg(agent_name),
    updated_at = now()
FROM deliveries d
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND b.session_id = d.session_id
  AND b.kagent_session_id IS NULL
  AND b.kagent_request_id IS NOT DISTINCT FROM sqlc.narg(request_id)::text;

-- The bindings that sent a create whose Session id never came back and whose session has nothing
-- open any more, so nothing is about to retry the create. The workspace service owns the creates
-- of workspace bindings.
-- name: ListUnresolvedCreates :many
SELECT b.session_id, b.kind, b.role, b.mcp_grant_kind, b.kagent_create_request_id::text AS create_request_id,
       b.kagent_create_agent_namespace AS agent_namespace, b.kagent_create_agent_name AS agent_name
FROM agent_bindings b
WHERE b.kagent_create_request_id IS NOT NULL AND b.kagent_session_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = b.session_id)
  AND NOT EXISTS (
      SELECT 1 FROM deliveries d
      WHERE d.session_id = b.session_id AND d.state IN ('recorded', 'sending', 'delivered', 'uncertain'))
ORDER BY b.created_at
LIMIT $1;

-- Moves the binding off the request id of an unresolved create, so that a message that arrives while
-- the sweep replays and deletes that create's Session makes its own Session under a new id and
-- cannot bind the one being deleted. Matches only while the create is unresolved, nothing of the
-- session is open, and the create still names the binding's request id (a binding already moved
-- keeps its id and still matches, so a repeated pass replays the same create). Zero rows: the
-- create was settled, or a message is open; the sweep leaves it.
-- name: RotateCreateRequest :execrows
UPDATE agent_bindings b
SET kagent_request_id = CASE
        WHEN coalesce(nullif(b.kagent_request_id, ''), 'mainloop-session-' || b.session_id) = b.kagent_create_request_id
        THEN sqlc.arg(new_request_id)::text ELSE b.kagent_request_id END,
    updated_at = now()
WHERE b.session_id = sqlc.arg(session_id) AND b.kagent_create_request_id = sqlc.arg(create_request_id)
  AND b.kagent_session_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM deliveries d
      WHERE d.session_id = b.session_id AND d.state IN ('recorded', 'sending', 'delivered', 'uncertain'));

-- Marks a create resolved: kagent has no Session for the request id, or the Session it returned
-- was deleted. Only while the create is still unresolved and nothing of the session is open.
-- name: ResolveCreate :execrows
UPDATE agent_bindings b
SET kagent_create_request_id = NULL, kagent_create_agent_namespace = NULL, kagent_create_agent_name = NULL,
    updated_at = now()
WHERE b.session_id = sqlc.arg(session_id) AND b.kagent_create_request_id = sqlc.arg(create_request_id)
  AND b.kagent_session_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM deliveries d
      WHERE d.session_id = b.session_id AND d.state IN ('recorded', 'sending', 'delivered', 'uncertain'));

-- name: NoteCreateFailure :one
-- Counts a failed attempt to create the delivery's kagent Session and returns the count. Fenced on
-- the claim; no rows when the claim is lost. updated_at stays: a failure is not activity.
UPDATE deliveries
SET create_failures = create_failures + 1
WHERE id = $1 AND lease_owner = $2 AND attempts = $3
RETURNING create_failures;

-- name: MarkDeliverySending :execrows
-- Called in the sending-decision transaction after LockSessionTaskAttempt. The shared attempt
-- lock serializes this decision with stop; the native send follows only after commit.
UPDATE deliveries d
SET state = 'sending', detail = NULL, kagent_session_id = sqlc.arg(kagent_session_id),
    updated_at = now()
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND d.state = 'recorded'
  AND NOT EXISTS (
      SELECT 1 FROM task_attempts a WHERE a.session_id = d.session_id AND a.stop_state <> 'none'
  )
  AND EXISTS (
      SELECT 1 FROM sessions s WHERE s.id = d.session_id
      AND s.status NOT IN ('cancelled', 'failed') AND s.archived_at IS NULL
  );

-- name: MarkDeliveryDelivered :execrows
-- Outcome writes advance the durable version atomically with the state, including recovered
-- delivered tasks. A task id proves acceptance, not that native execution has finished.
UPDATE deliveries
SET state = 'delivered', task_id = $4, evidence_ref = $5, detail = NULL,
    outcome_version = outcome_version + 1, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('sending', 'uncertain', 'delivered');

-- name: MarkDeliveryUncertain :execrows
UPDATE deliveries
SET state = 'uncertain', detail = $4, outcome_version = outcome_version + 1, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('recorded', 'sending', 'delivered', 'uncertain');

-- name: NoteHoldPassed :execrows
-- Records, once, that an uncertain delivery's hold is over. updated_at stays: the hold is counted
-- from it. Zero rows when the note is already there, so the caller announces it once.
UPDATE deliveries
SET detail = $4
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'uncertain'
  AND detail IS DISTINCT FROM $4;

-- name: FinishDelivery :execrows
UPDATE deliveries
SET state = $4, detail = $5, task_id = coalesce(sqlc.narg(task_id), task_id),
    evidence_ref = coalesce(sqlc.narg(evidence_ref), evidence_ref),
    lease_owner = NULL, lease_until = NULL, outcome_version = outcome_version + 1, updated_at = now()
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

-- name: GetDeliveryActivity :one
-- The newest update of any delivery of the session, or the epoch when it has none.
SELECT coalesce(max(updated_at), 'epoch'::timestamptz)::timestamptz AS activity
FROM deliveries WHERE session_id = $1;

-- name: EndDelivery :one
-- Ends a delivery from outside its claim (a stop, a cancel) and releases the claim, so a loop that
-- still holds it finds its next write matching nothing. Only from the given states (the ones the
-- caller observed), so the loser of a race with the loop changes nothing (no rows). Returns the
-- state the delivery was in, which says whether a send could have reached kagent.
WITH old AS (
    SELECT id, state FROM deliveries
    WHERE deliveries.id = sqlc.arg(id) AND deliveries.state = ANY(sqlc.arg(from_states)::text[])
    FOR UPDATE
)
UPDATE deliveries d
SET state = sqlc.arg(state), detail = sqlc.arg(detail),
    task_id = coalesce(sqlc.narg(task_id), d.task_id),
    evidence_ref = coalesce(sqlc.narg(evidence_ref), d.evidence_ref),
    lease_owner = NULL, lease_until = NULL, outcome_version = d.outcome_version + 1, updated_at = now()
FROM old
WHERE d.id = old.id
RETURNING old.state::text AS previous_state;

-- name: ListOpenDeliveries :many
-- The deliveries a stop or a cancel works on: open, or uncertain (a task may be running).
SELECT * FROM deliveries
WHERE session_id = $1 AND state IN ('recorded', 'sending', 'delivered', 'uncertain')
ORDER BY created_at, id;

-- name: GetSessionRow :one
-- A session with what its binding says about it. The owner filter is part of the key.
SELECT s.*, b.kind AS agent_kind, b.parent_session_id, t.name AS topic
FROM sessions s
LEFT JOIN agent_bindings b ON b.session_id = s.id
LEFT JOIN topics t ON t.id = b.topic_id
WHERE s.id = $1 AND s.user_id = $2;

-- name: ListSessionRows :many
-- The owner's unarchived sessions other than the main thread, newest first: every session that is
-- not history, plus the newest 100 history records (completed, failed, cancelled, superseded).
-- Optionally of one status.
SELECT s.*, b.kind AS agent_kind, b.parent_session_id, t.name AS topic
FROM sessions s
LEFT JOIN agent_bindings b ON b.session_id = s.id
LEFT JOIN topics t ON t.id = b.topic_id
WHERE s.user_id = $1 AND s.archived_at IS NULL AND b.role IS DISTINCT FROM 'main'
  AND (sqlc.narg(status)::text IS NULL OR s.status = sqlc.narg(status)::text)
  AND (s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
       OR s.id IN (
           SELECT h.id FROM sessions h
           LEFT JOIN agent_bindings hb ON hb.session_id = h.id
           WHERE h.user_id = $1 AND h.archived_at IS NULL AND hb.role IS DISTINCT FROM 'main'
             AND h.status IN ('completed', 'failed', 'cancelled', 'superseded')
           ORDER BY h.created_at DESC, h.id LIMIT 100))
ORDER BY s.created_at DESC, s.id;

-- name: CountSessions :one
SELECT count(*) FROM sessions WHERE user_id = $1;

-- name: SetSessionCancelled :one
-- The cancelled status is final (RefreshSessionStatus leaves it alone). Nothing changes once the
-- session is finished.
UPDATE sessions
SET status = 'cancelled', completed_at = coalesce(completed_at, now()), agent_cancel_outcome = 'unknown',
    agent_cancel_attempts = 0, agent_cancel_next_attempt_at = now() + interval '30 seconds'
WHERE id = $1 AND status NOT IN ('completed', 'failed', 'cancelled')
RETURNING id;

-- name: RecordSessionCancelOutcome :exec
UPDATE sessions SET agent_cancel_outcome = $2
WHERE id = $1 AND status = 'cancelled' AND agent_cancel_outcome = 'unknown';

-- name: ListSessionKagentContexts :many
-- Include replaced contexts: a delivery may still have native work on an older Session.
SELECT kagent_session_id::text FROM agent_bindings
WHERE agent_bindings.session_id = $1 AND kagent_session_id IS NOT NULL AND kagent_session_id <> ''
UNION
SELECT kagent_session_id::text FROM deliveries
WHERE deliveries.session_id = $1 AND kagent_session_id IS NOT NULL AND kagent_session_id <> '';

-- name: GetSessionScheduledExecution :one
SELECT execution_id, schedule_id, kagent_session_id FROM scheduled_run_links WHERE session_id = $1;

-- name: ListScheduledSessionCancelsDue :many
SELECT s.id FROM sessions s
WHERE s.user_id = $1 AND s.status = 'cancelled' AND s.agent_cancel_outcome = 'unknown'
  AND s.agent_cancel_next_attempt_at <= now()
  AND EXISTS (SELECT 1 FROM scheduled_run_links l WHERE l.session_id = s.id)
ORDER BY s.agent_cancel_next_attempt_at, s.id LIMIT $2;

-- name: BeginScheduledSessionCancelReconcile :execrows
-- Commit the next retry before remote I/O, including when this pass crashes. A bounded call ends
-- before the earliest next retry. No transaction stays open across the native observation.
UPDATE sessions
SET agent_cancel_next_attempt_at = now() + make_interval(secs => least(30 * power(2, agent_cancel_attempts), 300)::float8),
    agent_cancel_attempts = least(agent_cancel_attempts + 1, 5)
WHERE id = $1 AND status = 'cancelled' AND agent_cancel_outcome = 'unknown'
  AND agent_cancel_next_attempt_at <= now();

-- name: ListCancelledSessionSends :many
-- Preserve unresolved Mainloop sends as uncertainty when reconciling a scheduled cancellation.
SELECT * FROM deliveries WHERE session_id = $1 AND state = 'cancelled' AND detail = 'cancelled by user'
  AND (kagent_session_id IS NOT NULL OR task_id IS NOT NULL);

-- name: SetBindingAgentRef :exec
UPDATE agent_bindings
SET kagent_agent_namespace = $2, kagent_agent_name = $3, updated_at = now()
WHERE session_id = $1;

-- name: ArchiveFinishedSession :execrows
UPDATE sessions SET archived_at = coalesce(archived_at, now())
WHERE id = $1 AND status IN ('completed', 'failed', 'cancelled');
