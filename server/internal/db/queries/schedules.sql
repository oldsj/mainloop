-- Scheduled runs: the schedule registry and the per-execution links (internal/schedules).

-- Records a schedule Mainloop created or found. A replay of the create (same request id) finds the
-- row; one the sweep found first is given the request id.
-- name: UpsertScheduledRunSchedule :execrows
INSERT INTO scheduled_run_schedules (
    schedule_id, owner_id, agent_namespace, agent_name, request_id, adopt_from
) VALUES ($1, $2, $3, $4, sqlc.narg(request_id), $5)
ON CONFLICT (schedule_id) DO UPDATE
SET request_id = coalesce(scheduled_run_schedules.request_id, EXCLUDED.request_id),
    updated_at = now()
WHERE scheduled_run_schedules.request_id IS NULL AND EXCLUDED.request_id IS NOT NULL;

-- name: GetScheduledRunSchedule :one
SELECT * FROM scheduled_run_schedules WHERE schedule_id = $1 AND owner_id = $2;

-- name: ListScheduledRunSchedulesToAdopt :many
SELECT * FROM scheduled_run_schedules
WHERE owner_id = $1 AND settled_at IS NULL
ORDER BY schedule_id;

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
