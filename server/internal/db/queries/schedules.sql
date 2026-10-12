-- Scheduled runs: the create intent / schedule registry and the per-execution links (internal/schedules).

-- The intent is written before the create is sent; a replay of the same request id finds the row.
-- name: InsertScheduledRunIntent :execrows
INSERT INTO scheduled_run_schedules (request_id, owner_id, agent_namespace, agent_name, create_config)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (request_id) DO NOTHING;

-- name: GetScheduledRunByRequest :one
SELECT * FROM scheduled_run_schedules WHERE request_id = $1 AND owner_id = $2;

-- kagent answered the create: the schedule is ours, and no attempt's lease matters any more.
-- name: ConfirmScheduledRunCreate :execrows
UPDATE scheduled_run_schedules
SET state = 'created', schedule_id = $2, updated_at = now()
WHERE request_id = $1 AND state = 'intent';

-- name: DeleteScheduledRunAttempts :exec
DELETE FROM scheduled_run_create_attempts WHERE request_id = $1;

-- A create attempt takes a lease of bound_secs on its intent. No row back: the request is not an
-- intent (confirmed, or deleted by the sweep).
-- name: BeginScheduledRunAttempt :one
INSERT INTO scheduled_run_create_attempts (request_id, expires_at)
SELECT s.request_id, now() + make_interval(secs => sqlc.arg(bound_secs)::float8)
FROM scheduled_run_schedules s
WHERE s.request_id = $1 AND s.state = 'intent'
RETURNING id;

-- The recovery's lease: only when no other attempt's lease is live.
-- name: BeginRecoveryAttempt :one
INSERT INTO scheduled_run_create_attempts (request_id, expires_at)
SELECT s.request_id, now() + make_interval(secs => sqlc.arg(bound_secs)::float8)
FROM scheduled_run_schedules s
WHERE s.request_id = $1 AND s.state = 'intent'
    AND NOT EXISTS (
        SELECT 1 FROM scheduled_run_create_attempts a WHERE a.request_id = s.request_id AND a.expires_at > now()
    )
RETURNING id;

-- name: DeleteExpiredScheduledRunAttempts :exec
DELETE FROM scheduled_run_create_attempts WHERE request_id = $1 AND expires_at <= now();

-- name: EndScheduledRunAttempt :exec
DELETE FROM scheduled_run_create_attempts WHERE id = $1;

-- The recovery decides a refusal is final. The intent row is locked first, which also holds off any
-- attempt that would take a lease (its foreign key waits), so the live-lease count cannot change under it.
-- name: LockScheduledRunIntent :one
SELECT request_id FROM scheduled_run_schedules WHERE request_id = $1 AND state = 'intent' FOR UPDATE;

-- name: CountOtherLiveScheduledRunAttempts :one
SELECT count(*) FROM scheduled_run_create_attempts WHERE request_id = $1 AND id <> $2 AND expires_at > now();

-- name: DeleteScheduledRunIntent :execrows
DELETE FROM scheduled_run_schedules WHERE request_id = $1 AND state = 'intent';

-- Creates that may have been sent and not confirmed, with no live attempt.
-- name: ListScheduledRunIntents :many
SELECT s.* FROM scheduled_run_schedules s
WHERE s.owner_id = $1 AND s.state = 'intent'
    AND NOT EXISTS (
        SELECT 1 FROM scheduled_run_create_attempts a WHERE a.request_id = s.request_id AND a.expires_at > now()
    )
ORDER BY s.created_at;

-- name: GetScheduledRunSchedule :one
SELECT * FROM scheduled_run_schedules WHERE schedule_id = $1 AND owner_id = $2;

-- name: ListScheduledRunSchedulesByID :many
SELECT * FROM scheduled_run_schedules WHERE owner_id = $1 AND schedule_id = ANY(sqlc.arg(schedule_ids)::text[]);

-- name: ListScheduledRunSchedulesToAdopt :many
SELECT * FROM scheduled_run_schedules
WHERE owner_id = $1 AND state = 'created' AND settled_at IS NULL
ORDER BY schedule_id;

-- Every Agent a create was ever sent to: with the configured Agents, the set this deployment owns.
-- name: ListScheduleOwnedAgents :many
SELECT namespace, name FROM kagent_agents;

-- name: MarkScheduledRunScheduleDeleted :exec
UPDATE scheduled_run_schedules
SET deleted_at = coalesce(deleted_at, now()), updated_at = now()
WHERE schedule_id = $1;

-- The sweep read a deleted schedule's executions to the end.
-- name: SettleScheduledRunSchedule :exec
UPDATE scheduled_run_schedules
SET settled_at = coalesce(settled_at, now()), updated_at = now()
WHERE schedule_id = $1 AND deleted_at IS NOT NULL;

-- name: SetScheduledRunCursor :exec
UPDATE scheduled_run_schedules
SET cursor_execution_id = $2, updated_at = now()
WHERE schedule_id = $1 AND cursor_execution_id < $2;

-- name: ListScheduledRunLinks :many
SELECT * FROM scheduled_run_links WHERE schedule_id = $1 AND execution_id = ANY(sqlc.arg(execution_ids)::text[]);

-- name: GetScheduledRunLink :one
SELECT * FROM scheduled_run_links WHERE execution_id = $1 FOR UPDATE;

-- The link is the claim: only the transaction that inserts it goes on to make the session.
-- name: InsertScheduledRunLink :execrows
INSERT INTO scheduled_run_links (execution_id, schedule_id, kagent_session_id, session_id, state)
VALUES ($1, $2, sqlc.narg(kagent_session_id), sqlc.narg(session_id), $3)
ON CONFLICT (execution_id) DO NOTHING;

-- name: SetScheduledRunLinkSession :execrows
UPDATE scheduled_run_links
SET kagent_session_id = $2, session_id = $3, updated_at = now()
WHERE execution_id = $1 AND session_id IS NULL;

-- name: SetScheduledRunLinkState :exec
UPDATE scheduled_run_links SET state = $2, updated_at = now() WHERE execution_id = $1;

-- name: MarkScheduledRunLinkAttention :exec
UPDATE scheduled_run_links
SET attention_opened_at = coalesce(attention_opened_at, now()), updated_at = now()
WHERE execution_id = $1;

-- The adopted session follows its execution to an end, unless something else already moved it.
-- name: EndScheduledRunSession :exec
UPDATE sessions
SET status = $2, error = sqlc.narg(error), completed_at = coalesce(completed_at, now())
WHERE id = $1 AND status IN ('pending', 'active');

-- name: StartScheduledRunSession :exec
UPDATE sessions
SET status = 'active', started_at = coalesce(started_at, now())
WHERE id = $1 AND status = 'pending';
