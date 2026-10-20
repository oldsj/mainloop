-- Main thread, message mirror and delivery ledger (internal/sessions).
--
-- The "fenced" queries record a claimer's outcome: they match only the row's current claim
-- (id, lease_owner, attempts) and report rows affected, so a claimer that lost its lease changes
-- nothing. The ones that move to a final state also release the lease.

-- name: GetMainBinding :one
SELECT b.*, s.user_id, s.title, s.status AS session_status
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE b.role = 'main' AND b.main_released_at IS NULL AND s.user_id = $1 AND s.archived_at IS NULL
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

-- Confirms the session is the owner's and names its provider profile, for /model and /effort: a
-- model's format is checked at the API boundary, kagent is the real gate, but an effort level is
-- checked here against the session's provider profile's own effort_levels before it is stored.
-- name: GetBindingKindOwned :one
SELECT b.kind FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE s.id = sqlc.arg(session_id) AND s.user_id = sqlc.arg(user_id);

-- Sets, or clears (NULL), a session's /model override. Ownership is already confirmed by the
-- caller's GetBindingKindOwned read; ":one" with no matching row (an id a concurrent delete just
-- removed) reads as pgx.ErrNoRows, the same not-found the read would have given.
-- name: SetBindingModelOverride :one
UPDATE agent_bindings
SET model_override = sqlc.narg(model_override)::text,
    model_effort_touched = model_effort_touched OR sqlc.narg(model_override)::text IS NOT NULL,
    updated_at = now()
WHERE session_id = sqlc.arg(session_id)
RETURNING session_id, model_override, effort_override;

-- Sets, or clears (NULL), a session's /effort override. See SetBindingModelOverride.
-- name: SetBindingEffortOverride :one
UPDATE agent_bindings
SET effort_override = sqlc.narg(effort_override)::text,
    model_effort_touched = model_effort_touched OR sqlc.narg(effort_override)::text IS NOT NULL,
    updated_at = now()
WHERE session_id = sqlc.arg(session_id)
RETURNING session_id, model_override, effort_override;

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

-- An image pasted into the main chat composer, uploaded (unclaimed) ahead of the message that
-- attaches it.
-- name: InsertMessageImage :exec
INSERT INTO message_images (id, session_id, media_type, data, size_bytes)
VALUES ($1, $2, $3, $4, $5);

-- Claims the given uploads for the message, idempotently: a retry of the same message id matches
-- rows it already claimed (message_id = the same message), not just unclaimed ones. The caller
-- compares the returned ids against what it asked for; fewer means some id was unknown, belonged
-- to another session, or was already claimed by a different message.
-- name: AttachMessageImages :many
UPDATE message_images
SET message_id = sqlc.arg(message_id)::text
WHERE id = ANY(sqlc.arg(ids)::text[]) AND session_id = sqlc.arg(session_id)
  AND (message_id IS NULL OR message_id = sqlc.arg(message_id)::text)
RETURNING id;

-- The images of one message, oldest first, built into the outbound turn alongside its text.
-- name: ListMessageImagesByMessage :many
SELECT id, media_type, data FROM message_images
WHERE message_id = sqlc.arg(message_id)::text
ORDER BY created_at, id;

-- The images of every message in a conversation, oldest first, grouped by the caller.
-- name: ListMessageImagesBySession :many
SELECT id, message_id, media_type, data FROM message_images
WHERE session_id = sqlc.arg(session_id) AND message_id IS NOT NULL
ORDER BY created_at, id;

-- A user message carries its delivery's source, so who wrote it covers the whole history, not just
-- the newest deliveries. Assistant messages have no delivery.
-- name: ListSessionMessages :many
SELECT m.id, m.session_id, m.role, m.content, m.created_at,
       coalesce(d.source, '')::text AS source
FROM session_messages m
LEFT JOIN deliveries d ON d.id = m.id
WHERE m.session_id = $1
ORDER BY m.created_at, m.id;

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

-- A message durably recorded while another turn is open. The partial index behind
-- deliveries_one_open excludes this state, so many may queue up; PromoteNextQueuedDelivery admits
-- them in the order they arrived, once each turn ends.
-- name: InsertQueuedDelivery :one
INSERT INTO deliveries (id, idempotency_key, session_id, source, state)
VALUES ($1, $1, $2, $3, 'queued')
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- Admits the oldest queued message, but only once the session truly has no open turn left: the
-- delivery that just ended may not have been the session's only open one (an uncertain delivery
-- still inside its hold may be a newer turn that is still running behind the one that just ended).
-- The blocking predicate matches GetBlockingDelivery, so promotion never races ahead of a turn that
-- sends still queue behind. Promoting while another delivery holds the one-open-turn slot would
-- violate deliveries_one_open and roll back the caller's transaction. Zero rows when nothing is
-- queued or the slot is still held. The caller holds mainloop:send:<session_id> for the duration.
-- name: PromoteNextQueuedDelivery :one
WITH next AS (
    SELECT q.id FROM deliveries q
    WHERE q.session_id = sqlc.arg(session_id) AND q.state = 'queued'
      AND NOT EXISTS (
          SELECT 1 FROM deliveries o
          WHERE o.session_id = q.session_id
            AND (o.state IN ('recorded', 'sending', 'delivered')
                 OR (o.state = 'uncertain' AND o.updated_at > now() - make_interval(secs => sqlc.arg(hold_seconds)::float8)))
      )
    ORDER BY q.created_at, q.id
    LIMIT 1
    FOR UPDATE
)
UPDATE deliveries d
SET state = 'recorded', updated_at = now()
FROM next
WHERE d.id = next.id
RETURNING d.id;

-- Whether a message is already waiting behind the open turn. Admission queues a new send behind
-- any existing queued row, not just an open one, so the backlog stays in order even once the turn
-- that first blocked it has gone past its uncertainty hold.
-- name: HasQueuedDelivery :one
SELECT EXISTS (SELECT 1 FROM deliveries WHERE session_id = sqlc.arg(session_id) AND state = 'queued');

-- Sessions with a queued delivery that nothing is blocking any more: the turn that queued it has
-- ended or gone past its uncertainty hold, but the backlog was never released (a crash between the
-- admission decision and the promotion, or a settle that ran before the queued row was visible).
-- The recovery sweep promotes these; a normal send or settle releases its own session's backlog as
-- it goes, so this is a safety net, not the common path.
-- name: ListStrandedQueuedSessions :many
SELECT DISTINCT q.session_id FROM deliveries q
WHERE q.state = 'queued'
  AND NOT EXISTS (
      SELECT 1 FROM deliveries o
      WHERE o.session_id = q.session_id
        AND (o.state IN ('recorded', 'sending', 'delivered')
             OR (o.state = 'uncertain' AND o.updated_at > now() - make_interval(secs => sqlc.arg(hold_seconds)::float8)))
  )
LIMIT sqlc.arg(limit_count);

-- A session that is cancelled outright takes no more messages: every message still queued behind
-- its last open turn is cancelled with it, not admitted (Cancel ends the whole session, not one
-- turn).
-- name: CancelQueuedDeliveries :many
UPDATE deliveries
SET state = 'cancelled', detail = sqlc.arg(detail), outcome_version = outcome_version + 1, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND state = 'queued'
RETURNING id;

-- name: GetDelivery :one
SELECT * FROM deliveries WHERE id = $1;

-- name: ListSessionDeliveries :many
SELECT * FROM deliveries WHERE session_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2;

-- The deliveries that can attribute a mirrored reply to a provider: deliver.go sets
-- kagent_session_id when it sends the message, naming the Session the turn ran on, and derives the
-- reply's own id from this row (its task id, or its own id for a cancelled turn's stop note) rather
-- than storing it. The caller recomputes that id to match it to a stored message.
-- name: ListSessionDeliveriesForProvider :many
SELECT id, task_id, state, kagent_session_id FROM deliveries
WHERE session_id = $1 AND kagent_session_id IS NOT NULL;

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
       d.kagent_session_id AS sent_kagent_session_id, d.sent_text,
       d.activity_at, d.activity_kind, d.stalled_at,
       b.session_id AS binding_session_id, b.kagent_session_id, b.kagent_request_id,
       b.kind AS binding_kind, coalesce(b.role::text, '')::text AS binding_role,
       b.credential_identity,
       b.model_override, b.effort_override, coalesce(b.model_effort_touched, false) AS model_effort_touched,
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
SET kagent_session_id = NULL, kagent_request_id = sqlc.arg(new_request_id), standing_hash = NULL, guidance_version = NULL, guidance_delivery_id = NULL,
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
    -- A main pins the Agent its first create names, so a later configuration change moves no existing
    -- main (a switch pins its target in BeginMainSwitch).
    kagent_agent_namespace = CASE WHEN b.role = 'main' THEN coalesce(b.kagent_agent_namespace, sqlc.arg(agent_namespace)) ELSE b.kagent_agent_namespace END,
    kagent_agent_name = CASE WHEN b.role = 'main' THEN coalesce(b.kagent_agent_name, sqlc.arg(agent_name)) ELSE b.kagent_agent_name END,
    updated_at = now()
FROM deliveries d
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND b.session_id = d.session_id
  AND b.kagent_session_id IS NULL
  AND b.kagent_request_id IS NOT DISTINCT FROM sqlc.narg(request_id)::text;

-- A first-use main pins the Agent its first create names (MarkCreateRequested). When kagent
-- proves that create was refused before it reserved anything, the pin was never confirmed by a
-- Session, so it is dropped and the next create follows the configured Agent again. A main that has
-- had a Session (a replacement has a request id of its own) or a switch in flight keeps its pin.
-- Fenced like MarkCreateRequested.
-- name: ClearUnprovenMainPin :execrows
UPDATE agent_bindings b
SET kagent_agent_namespace = NULL, kagent_agent_name = NULL, updated_at = now()
FROM deliveries d
WHERE d.id = sqlc.arg(id) AND d.lease_owner = sqlc.arg(lease_owner) AND d.attempts = sqlc.arg(attempts)
  AND b.session_id = d.session_id
  AND b.role = 'main' AND NOT b.main_pending
  AND b.kagent_session_id IS NULL AND b.kagent_request_id IS NULL
  AND b.kagent_agent_namespace = sqlc.arg(agent_namespace) AND b.kagent_agent_name = sqlc.arg(agent_name);

-- The bindings that sent a create whose Session id never came back and whose session has nothing
-- open any more, so nothing is about to retry the create. The workspace service owns the creates
-- of workspace bindings.
-- name: ListUnresolvedCreates :many
SELECT b.session_id, b.kind, b.role, b.mcp_grant_kind, b.credential_identity, b.kagent_create_request_id::text AS create_request_id,
       b.kagent_create_agent_namespace AS agent_namespace, b.kagent_create_agent_name AS agent_name
FROM agent_bindings b
WHERE b.kagent_create_request_id IS NOT NULL AND b.kagent_session_id IS NULL
  AND NOT b.main_pending
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
  AND b.kagent_session_id IS NULL AND NOT b.main_pending
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
  AND b.kagent_session_id IS NULL AND NOT b.main_pending
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
    sent_text = coalesce(sqlc.narg(sent_text)::text, d.sent_text), updated_at = now()
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

-- name: SetSessionEnded :one
-- Ends a session as completed, failed or cancelled. That status is final (RefreshSessionStatus
-- leaves it alone). Nothing changes once the session is finished.
UPDATE sessions
SET status = sqlc.arg(status), completed_at = coalesce(completed_at, now()), agent_cancel_outcome = 'unknown',
    agent_cancel_attempts = 0, agent_cancel_next_attempt_at = now() + interval '30 seconds'
WHERE id = sqlc.arg(id) AND status NOT IN ('completed', 'failed', 'cancelled')
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

-- Main provider selection (internal/sessions/provider.go).
--
-- A switch changes the binding in two steps around the kagent create: the intent (target provider,
-- Agent, request id and the frozen create contract) commits first, then the Session id and the
-- ready flag are stored. Every write after the intent matches the request id, so a preparation
-- that was superseded by a restart changes nothing.

-- name: CreateMainBinding :one
-- A first-use main follows the configured Agent of its provider until a switch pins one.
INSERT INTO agent_bindings (session_id, kind, role, mcp_grant_kind)
VALUES ($1, $2, 'main', 'coordination')
RETURNING *;

-- Hands the owner slot to a replacement: the old row stays for audit.
-- name: ReleaseMainBinding :execrows
UPDATE agent_bindings
SET main_released_at = now(), main_pending = false, updated_at = now()
WHERE session_id = $1 AND role = 'main' AND main_released_at IS NULL;

-- name: InsertMainProviderHistory :exec
INSERT INTO main_provider_history (
    session_id, provider, kagent_session_id, agent_namespace, agent_name, request_id, create_request_id,
    model, turns, abandoned, error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ListMainProviderHistory :many
SELECT * FROM main_provider_history WHERE session_id = $1 ORDER BY retired_at, id;

-- name: LockMainBinding :one
-- The main binding row, locked for the intent transaction. Matches only the binding the caller read.
SELECT * FROM agent_bindings
WHERE session_id = $1 AND role = 'main' AND main_released_at IS NULL
FOR UPDATE;

-- Commits a switch intent. The caller holds the binding row lock and has read the identity it
-- retires; the new identity is a clean one: no Session, no standing hash, no turns.
-- name: BeginMainSwitch :execrows
UPDATE agent_bindings
SET kind = sqlc.arg(kind),
    kagent_agent_namespace = sqlc.arg(agent_namespace), kagent_agent_name = sqlc.arg(agent_name),
    kagent_session_id = NULL, kagent_request_id = sqlc.arg(request_id),
    credential_identity = sqlc.arg(request_id),
    kagent_create_request_id = sqlc.arg(request_id),
    kagent_create_agent_namespace = sqlc.arg(agent_namespace), kagent_create_agent_name = sqlc.arg(agent_name),
    standing_hash = NULL, guidance_version = NULL, guidance_delivery_id = NULL, model = NULL, turns = 0, queue_held = false,
    main_pending = true, main_error = NULL, main_recoverable = false, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND role = 'main' AND main_released_at IS NULL;

-- Stores the Session id the target's create returned. The binding stays pending.
-- name: SetMainTargetSession :execrows
UPDATE agent_bindings
SET kagent_session_id = sqlc.arg(kagent_session_id), updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND main_pending
  AND kagent_session_id IS NULL AND kagent_request_id = sqlc.arg(request_id)::text;

-- name: FinishMainSwitch :execrows
UPDATE agent_bindings
SET main_pending = false, main_error = NULL, main_recoverable = false, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND main_pending
  AND kagent_session_id IS NOT DISTINCT FROM sqlc.narg(kagent_session_id)::text
  AND kagent_request_id = sqlc.arg(request_id)::text;

-- name: FailMainSwitch :execrows
UPDATE agent_bindings
SET main_error = sqlc.arg(error), main_recoverable = main_recoverable OR sqlc.arg(recoverable)::boolean,
    updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND main_pending AND kagent_request_id = sqlc.arg(request_id)::text;

-- name: ClearMainSwitchError :execrows
UPDATE agent_bindings SET main_error = NULL, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND main_pending AND kagent_request_id = sqlc.arg(request_id)::text;

-- name: CountMainBlockingWork :one
-- Work that stops a provider switch: an open or uncertain delivery, a message still queued behind
-- one, or an unconfirmed cancel.
SELECT
    (SELECT count(*) FROM deliveries d
      WHERE d.session_id = sqlc.arg(session_id) AND d.state IN ('recorded', 'sending', 'delivered', 'uncertain', 'queued'))::bigint AS deliveries,
    (SELECT count(*) FROM sessions s
      WHERE s.id = sqlc.arg(session_id) AND s.agent_cancel_outcome = 'unknown')::bigint AS cancels;

-- The newest completed deliveries of a session, newest first. Each is one user message and at most
-- one mirrored reply, so the last N messages of the conversation come from the last N of them.
-- name: ListCompletedDeliveries :many
SELECT id, task_id FROM deliveries
WHERE session_id = $1 AND state = 'completed'
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListSessionMessagesByID :many
SELECT * FROM session_messages WHERE session_id = sqlc.arg(session_id) AND id = ANY(sqlc.arg(ids)::text[]) ORDER BY created_at, id;

-- Marks the main binding's Session as having been given its standing context and the current
-- guidance version, in the transaction that freezes the delivery's text. Zero rows when the binding
-- already has one.
-- name: SetBindingStandingHash :execrows
UPDATE agent_bindings SET standing_hash = sqlc.arg(standing_hash), guidance_version = sqlc.arg(guidance_version)::integer,
    guidance_delivery_id = sqlc.arg(delivery_id)::text, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND standing_hash IS NULL;

-- Marks the Session as having been given the current guidance version alone, for a Session that
-- already has its standing context. Zero rows when it has none or already has this version.
-- name: SetBindingGuidanceVersion :execrows
UPDATE agent_bindings SET guidance_version = sqlc.arg(guidance_version)::integer, guidance_delivery_id = sqlc.arg(delivery_id)::text,
    updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND standing_hash IS NOT NULL
  AND guidance_version IS DISTINCT FROM sqlc.arg(guidance_version)::integer;

-- A delivery that ends without kagent having seen its message gives back the standing context it
-- froze, so the next message to the Session carries it again. Only the hash of this delivery's own
-- freeze (its id and frozen text) is cleared.
-- name: ClearBindingStandingHash :exec
UPDATE agent_bindings b SET standing_hash = NULL, updated_at = now()
FROM deliveries d
WHERE d.id = $1 AND b.session_id = d.session_id AND d.sent_text IS NOT NULL
  AND b.standing_hash = encode(sha256(convert_to(d.id || E'\n' || d.sent_text, 'UTF8')), 'hex');

-- A delivery that ends without kagent having seen its message gives back the guidance version it
-- delivered, so the next message to the Session carries the context block again. Only the version
-- this delivery recorded is cleared.
-- name: ClearBindingGuidanceVersion :exec
UPDATE agent_bindings b SET guidance_version = NULL, guidance_delivery_id = NULL, updated_at = now()
WHERE b.guidance_delivery_id = $1;

-- A restart of a session whose turn cannot be stopped (restart.go). The row is written before the
-- native delete, and settled in the transaction that fails the turns that were open on the Session.
-- name: GetSessionRestart :one
SELECT * FROM session_restarts WHERE id = $1;

-- name: GetOpenSessionRestart :one
SELECT * FROM session_restarts WHERE session_id = $1 AND state = 'requested';

-- name: InsertSessionRestart :one
INSERT INTO session_restarts (id, session_id, kagent_session_id)
VALUES (sqlc.arg(id), sqlc.arg(session_id), sqlc.narg(kagent_session_id))
RETURNING *;

-- Records one unconfirmed delete. updated_at moves, so the resume sweep waits before it looks again.
-- name: NoteSessionRestartAttempt :exec
UPDATE session_restarts SET attempts = attempts + 1, detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested';

-- Zero rows when another caller settled it first.
-- name: FinishSessionRestart :execrows
UPDATE session_restarts
SET state = 'done', detail = NULL, failed_message_ids = sqlc.arg(failed_message_ids)::text[], updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested';

-- Requested restarts nobody has touched for a while: a request that was cut off, or a delete that
-- kagent did not confirm.
-- name: ListDueSessionRestarts :many
SELECT * FROM session_restarts
WHERE state = 'requested' AND updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
ORDER BY created_at, id
LIMIT sqlc.arg(limit_count);

-- Forgets the deleted Session so the next message creates a new one under a new request id and
-- carries the recent conversation again. Fenced on the Session the restart deleted: a binding that
-- has moved on to a newer Session is left alone.
-- name: ForgetDeletedKagentSession :execrows
UPDATE agent_bindings
SET kagent_session_id = NULL, kagent_request_id = sqlc.arg(new_request_id), standing_hash = NULL,
    guidance_version = NULL, guidance_delivery_id = NULL,
    kagent_create_request_id = NULL, kagent_create_agent_namespace = NULL, kagent_create_agent_name = NULL,
    updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND kagent_session_id = sqlc.arg(old_session_id)::text;

-- The turns a restart settles: every delivery that was sent, or may have been, to the deleted
-- Session (or to none). A recorded one has not been sent and is left for the next Session.
-- name: ListDeliveriesOnKagentSession :many
SELECT * FROM deliveries
WHERE session_id = sqlc.arg(session_id) AND state IN ('sending', 'delivered', 'uncertain')
  AND (kagent_session_id IS NULL OR kagent_session_id = sqlc.arg(kagent_session_id)::text)
ORDER BY created_at, id;

-- Stalled turns (stall.go). Records what the delivery loop last saw a turn's task do, and clears a
-- stall. Fenced on the claim like the outcome writes, but it is not an outcome: state,
-- outcome_version and updated_at stay. at is null for a kind change seen in a snapshot (the task as
-- it stood), which is not new activity. Returns whether the turn was marked stalled before.
-- name: NoteDeliveryActivity :one
WITH old AS (
    SELECT id, stalled_at FROM deliveries
    WHERE deliveries.id = sqlc.arg(id) AND deliveries.lease_owner = sqlc.arg(lease_owner)
      AND deliveries.attempts = sqlc.arg(attempts) AND deliveries.state IN ('sending', 'delivered')
    FOR UPDATE
)
UPDATE deliveries d
SET activity_at = coalesce(sqlc.narg(at)::timestamptz, d.activity_at),
    activity_kind = sqlc.arg(kind)::text,
    stalled_at = CASE WHEN sqlc.narg(at)::timestamptz IS NULL THEN d.stalled_at END
FROM old
WHERE d.id = old.id
RETURNING (old.stalled_at IS NOT NULL)::boolean AS was_stalled, d.session_id;

-- Marks every delivered turn that has been quiet past its allowance: tool_seconds while its last
-- event left a tool call running, seconds otherwise. A turn parked for the owner is never stalled.
-- Quiet is counted from the last live event, else from when the turn was marked delivered.
-- name: MarkStalledTurns :many
-- now is the caller's clock, so the allowance can be tested without waiting it out.
UPDATE deliveries d
SET stalled_at = sqlc.arg(now)::timestamptz
WHERE d.state = 'delivered' AND d.stalled_at IS NULL
  AND d.activity_kind IS DISTINCT FROM 'parked'
  AND coalesce(d.activity_at, d.updated_at) < sqlc.arg(now)::timestamptz - make_interval(secs => CASE
      WHEN d.activity_kind = 'tool' THEN sqlc.arg(tool_seconds)::float8 ELSE sqlc.arg(seconds)::float8 END)
RETURNING d.id, d.session_id, (SELECT s.user_id FROM sessions s WHERE s.id = d.session_id)::text AS user_id;

-- The session's stalled turn, if any: the delivered delivery the sweep marked.
-- name: GetStalledTurn :one
SELECT id, task_id, activity_at, activity_kind, stalled_at, updated_at FROM deliveries
WHERE session_id = $1 AND state = 'delivered' AND stalled_at IS NOT NULL
ORDER BY created_at DESC
LIMIT 1;
