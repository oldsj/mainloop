-- Privileged actions (docs/specs/privileged-actions.md).

-- Serialises requests of one owner, so the pending limits hold under concurrent calls.
-- name: LockPrivilegedRequests :exec
SELECT pg_advisory_xact_lock(hashtext('privileged_actions:' || sqlc.arg(owner_id)::text));

-- name: CountUndecidedPrivilegedActions :one
SELECT count(*) FILTER (WHERE requester_session_id = sqlc.arg(session_id))::int AS session_count,
       count(*)::int AS owner_count
FROM privileged_actions
WHERE owner_id = sqlc.arg(owner_id) AND state IN ('requested', 'pending_approval');

-- Counts the session's requests of the given types that a preflight refused in the last hour.
-- name: CountRefusedPrivilegedPreflights :one
SELECT count(*)::int
FROM privileged_action_events e
JOIN privileged_actions a ON a.id = e.action_id
WHERE a.owner_id = sqlc.arg(owner_id) AND a.requester_session_id = sqlc.arg(session_id)
  AND a.type = ANY (sqlc.arg(types)::text[])
  AND e.step = 'preflight_refused' AND e.created_at > now() - interval '1 hour';

-- A conflict (same session and request id, or an undecided action with the same arguments)
-- inserts nothing; the caller reads the existing row.
-- name: InsertPrivilegedAction :one
INSERT INTO privileged_actions (
    id, owner_id, request_id, requester_role, requester_session_id, requester_task_id,
    type, version, args, args_key, reason, state, expires_at
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_id), sqlc.arg(request_id), sqlc.arg(requester_role),
    sqlc.arg(requester_session_id), sqlc.narg(requester_task_id), sqlc.arg(type), sqlc.arg(version),
    sqlc.arg(args), sqlc.arg(args_key), sqlc.arg(reason), 'requested', sqlc.arg(expires_at)
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetPrivilegedAction :one
SELECT * FROM privileged_actions WHERE owner_id = sqlc.arg(owner_id) AND id = sqlc.arg(id);

-- name: GetPrivilegedActionByRequest :one
SELECT * FROM privileged_actions
WHERE requester_session_id = sqlc.arg(session_id) AND request_id = sqlc.arg(request_id);

-- name: GetOpenPrivilegedActionByArgs :one
SELECT * FROM privileged_actions
WHERE requester_session_id = sqlc.arg(session_id) AND args_key = sqlc.arg(args_key)
  AND state IN ('requested', 'pending_approval');

-- Requested actions of the given types to preflight, oldest first: untried ones once first_seconds
-- passed (serve-api gives its request call that long; serve-executor takes them at once), and ones
-- whose target could not be read idle_seconds after the last try.
-- name: ListUnpreflightedPrivilegedActions :many
SELECT * FROM privileged_actions
WHERE owner_id = sqlc.arg(owner_id) AND state = 'requested' AND type = ANY (sqlc.arg(types)::text[])
  AND updated_at <= now() - make_interval(secs => CASE WHEN preflight_attempts = 0
      THEN sqlc.arg(first_seconds)::double precision ELSE sqlc.arg(idle_seconds)::double precision END)
ORDER BY created_at, id
LIMIT 5;

-- Frozen actions waiting for the owner with no card yet: executor types, which serve-executor
-- froze without one.
-- name: ListUncardedPrivilegedActions :many
SELECT p.* FROM privileged_actions p
WHERE p.owner_id = sqlc.arg(owner_id) AND p.state = 'pending_approval'
  AND NOT EXISTS (
      SELECT 1 FROM attention_items a
      WHERE a.owner_id = p.owner_id AND a.source_kind = 'queue' AND a.source_id = 'privileged_action_' || p.id
  )
ORDER BY p.created_at, p.id
LIMIT 10;

-- Ends requested actions of the given (executor) types that nobody preflighted in time.
-- name: FailUnpickedPrivilegedActions :many
UPDATE privileged_actions
SET state = 'preflight_failed', last_error = sqlc.arg(last_error), finished_at = now(), updated_at = now(),
    delivery_state = 'pending', delivery_next_at = now()
WHERE owner_id = sqlc.arg(owner_id) AND state = 'requested' AND type = ANY (sqlc.arg(types)::text[])
  AND created_at < now() - make_interval(secs => sqlc.arg(pickup_seconds)::double precision)
RETURNING id;

-- Uncertain actions the owner has no error item for yet.
-- name: ListUnraisedUncertainPrivilegedActions :many
SELECT p.* FROM privileged_actions p
WHERE p.owner_id = sqlc.arg(owner_id) AND p.state = 'uncertain'
  AND NOT EXISTS (
      SELECT 1 FROM attention_items a
      WHERE a.owner_id = p.owner_id AND a.source_kind = 'queue'
        AND a.source_id = 'privileged_action_uncertain_' || p.id
  )
ORDER BY p.finished_at, p.id
LIMIT 10;

-- Records a preflight attempt that could not reach its facts; the row stays requested.
-- name: BumpPrivilegedPreflight :execrows
UPDATE privileged_actions
SET preflight_attempts = preflight_attempts + 1, last_error = sqlc.arg(last_error), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested';

-- Writes the frozen facts and digest once and makes the action a proposal.
-- name: FreezePrivilegedAction :one
UPDATE privileged_actions
SET frozen_facts = sqlc.arg(frozen_facts), digest = sqlc.arg(digest), state = 'pending_approval',
    last_error = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested'
RETURNING *;

-- Ends an action that never reached the owner: its preflight refused it.
-- name: FailPrivilegedPreflight :one
UPDATE privileged_actions
SET state = 'preflight_failed', last_error = sqlc.arg(last_error), finished_at = now(), updated_at = now(),
    delivery_state = CASE WHEN sqlc.arg(deliver)::boolean THEN 'pending' ELSE 'none' END,
    delivery_next_at = CASE WHEN sqlc.arg(deliver)::boolean THEN now() END
WHERE id = sqlc.arg(id) AND state = 'requested'
RETURNING *;

-- Records the owner's answers. Approve counts only with this action's digest in the answer's
-- metadata and before expiry; any other answer is a denial whose text is the note. A card closed
-- without an answer is a denial too.
-- name: DecideAnsweredPrivilegedActions :many
UPDATE privileged_actions p
SET state = CASE WHEN d.approve THEN 'approved' ELSE 'denied' END,
    decided_at = now(), updated_at = now(),
    approved_digest = CASE WHEN d.approve THEN p.digest END,
    next_attempt_at = CASE WHEN d.approve THEN now() END,
    decision_note = CASE WHEN d.approve THEN NULL ELSE d.note END,
    finished_at = CASE WHEN d.approve THEN NULL ELSE now() END,
    delivery_state = CASE WHEN d.approve THEN 'none' ELSE 'pending' END,
    delivery_next_at = CASE WHEN d.approve THEN NULL ELSE now() END
FROM (
    SELECT p2.id,
           (a.detail->>'response' = 'Approve' AND a.detail->'metadata'->>'reviewed_context' = p2.digest
            AND p2.expires_at > now()) AS approve,
           CASE
               WHEN NOT a.detail ? 'response' THEN 'the card was closed without an answer'
               WHEN a.detail->>'response' = 'Approve' AND p2.expires_at <= now() THEN 'the approval came after the request expired'
               WHEN a.detail->>'response' = 'Approve' THEN 'the approval did not carry this card''s digest'
               WHEN a.detail->>'response' = 'Deny' THEN NULL
               ELSE left(a.detail->>'response', 8192)
           END AS note
    FROM privileged_actions p2
    JOIN attention_items a ON a.owner_id = p2.owner_id AND a.source_kind = 'queue'
        AND a.source_id = 'privileged_action_' || p2.id
    WHERE p2.owner_id = sqlc.arg(owner_id) AND p2.state = 'pending_approval'
      AND (a.detail ? 'response' OR a.state <> 'open')
) d
WHERE p.id = d.id AND p.state = 'pending_approval'
RETURNING p.id, p.state, p.decision_note;

-- Expires undecided actions past their expiry, and approved ones no execution claimed within
-- claim_seconds of the approval.
-- name: ExpirePrivilegedActions :many
UPDATE privileged_actions
SET state = 'expired', finished_at = now(), updated_at = now(), next_attempt_at = NULL,
    delivery_state = 'pending', delivery_next_at = now()
WHERE owner_id = sqlc.arg(owner_id) AND (
    (state IN ('requested', 'pending_approval') AND expires_at <= now())
    OR (state = 'approved' AND attempts = 0
        AND decided_at < now() - make_interval(secs => sqlc.arg(claim_seconds)::double precision))
)
RETURNING id, state;

-- Closes the open card of every action no longer waiting for the owner (expired, cancelled).
-- name: ResolvePrivilegedActionCards :many
UPDATE attention_items a
SET state = 'resolved', resolved_at = now(), updated_at = now()
FROM privileged_actions p
WHERE a.owner_id = sqlc.arg(owner_id) AND a.source_kind = 'queue' AND a.state = 'open'
  AND a.source_id = 'privileged_action_' || p.id
  AND p.owner_id = a.owner_id AND p.state <> 'pending_approval'
RETURNING a.id;

-- Withdraws an undecided action of the calling session.
-- name: CancelPrivilegedAction :one
UPDATE privileged_actions
SET state = 'cancelled', finished_at = now(), updated_at = now()
WHERE owner_id = sqlc.arg(owner_id) AND id = sqlc.arg(id) AND requester_session_id = sqlc.arg(session_id)
  AND state IN ('requested', 'pending_approval')
RETURNING *;

-- Claims one approved action (or an executing one whose lease ran out) for an attempt.
-- name: ClaimPrivilegedAction :one
UPDATE privileged_actions
SET state = 'executing', attempts = attempts + 1, updated_at = now(),
    claimed_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision)
WHERE id = (
    SELECT c.id FROM privileged_actions c
    WHERE c.owner_id = sqlc.arg(owner_id) AND c.state IN ('approved', 'executing')
      AND c.type = ANY (sqlc.arg(types)::text[])
      AND (c.next_attempt_at IS NULL OR c.next_attempt_at <= now())
      AND (c.claimed_until IS NULL OR c.claimed_until < now())
    ORDER BY c.decided_at, c.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- Ends a claimed attempt with a final state and queues the outcome for the session. The attempt
-- count fences a holder whose lease ran out.
-- name: FinishPrivilegedAction :execrows
UPDATE privileged_actions
SET state = sqlc.arg(state), result = sqlc.narg(result), last_error = sqlc.narg(last_error),
    finished_at = now(), updated_at = now(), claimed_until = NULL, next_attempt_at = NULL,
    delivery_state = 'pending', delivery_next_at = now()
WHERE id = sqlc.arg(id) AND state = 'executing' AND attempts = sqlc.arg(attempts);

-- Leaves a claimed attempt for a later one, which observes before it writes.
-- name: RetryPrivilegedAction :execrows
UPDATE privileged_actions
SET last_error = sqlc.arg(last_error), updated_at = now(), claimed_until = NULL,
    next_attempt_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id) AND state = 'executing' AND attempts = sqlc.arg(attempts);

-- Claims one finished action whose outcome waits to be delivered to its session.
-- name: ClaimPrivilegedDelivery :one
UPDATE privileged_actions
SET delivery_attempts = delivery_attempts + 1, updated_at = now(),
    delivery_next_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision)
WHERE id = (
    SELECT d.id FROM privileged_actions d
    WHERE d.owner_id = sqlc.arg(owner_id) AND d.delivery_state = 'pending' AND d.delivery_next_at <= now()
    ORDER BY d.delivery_next_at, d.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: SettlePrivilegedDelivery :execrows
UPDATE privileged_actions
SET delivery_state = sqlc.arg(delivery_state), delivery_detail = sqlc.narg(delivery_detail), updated_at = now(),
    delivery_next_at = CASE WHEN sqlc.arg(delivery_state)::text = 'pending'
        THEN now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision) END
WHERE id = sqlc.arg(id) AND delivery_state = 'pending' AND delivery_attempts = sqlc.arg(delivery_attempts);

-- name: InsertPrivilegedActionEvent :exec
INSERT INTO privileged_action_events (action_id, step, detail)
VALUES (sqlc.arg(action_id), sqlc.arg(step), left(sqlc.arg(detail)::text, 4096));

-- name: ListPrivilegedActionEvents :many
SELECT * FROM privileged_action_events WHERE action_id = sqlc.arg(action_id) ORDER BY id;

-- name: ListRecentPrivilegedActions :many
-- The owner's audit, newest first.
SELECT * FROM privileged_actions WHERE owner_id = sqlc.arg(owner_id)
ORDER BY created_at DESC, id DESC
LIMIT 100;
