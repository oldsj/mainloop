-- Loops (docs/specs/loops.md).

-- name: InsertTaskLoop :one
INSERT INTO task_loops (
    id, owner_id, principal_key, request_id, recipient_key, subject_task_id, trigger, statuses,
    every_seconds, note, max_firings, min_interval_seconds, expires_at, baseline, fire_at, next_attempt_at,
    pr_conditions, quiet_seconds, quiet_since, expect_change, progress_mark
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_id), sqlc.arg(principal_key), sqlc.arg(request_id), sqlc.arg(recipient_key),
    sqlc.narg(subject_task_id), sqlc.arg(trigger), sqlc.arg(statuses)::text[], sqlc.narg(every_seconds),
    sqlc.arg(note), sqlc.arg(max_firings), sqlc.arg(min_interval_seconds), sqlc.arg(expires_at),
    sqlc.narg(baseline), sqlc.narg(fire_at), sqlc.arg(next_attempt_at), sqlc.arg(pr_conditions)::text[],
    sqlc.narg(quiet_seconds), sqlc.narg(quiet_since), sqlc.arg(expect_change), sqlc.narg(progress_mark)
)
RETURNING *;

-- name: GetTaskLoopByRequest :one
SELECT * FROM task_loops WHERE principal_key = $1 AND request_id = $2;

-- name: GetTaskLoopByID :one
SELECT * FROM task_loops WHERE id = $1;

-- An evaluation holds the loop row for its whole transaction: the subject's nudge trigger waits on
-- it, so a change is either seen by this evaluation or wakes the next one.
-- name: GetTaskLoopForUpdate :one
SELECT * FROM task_loops WHERE id = $1 FOR UPDATE;

-- Arming takes this lock first, so the count below holds for the insert that follows it.
-- name: CountArmedTaskLoops :one
SELECT count(*) FROM task_loops WHERE recipient_key = $1 AND state = 'armed';

-- A recipient's loops, armed ones first, then the latest that ended.
-- name: ListTaskLoops :many
SELECT * FROM task_loops
WHERE owner_id = sqlc.arg(owner_id) AND recipient_key = sqlc.arg(recipient_key)
ORDER BY (state = 'armed') DESC, created_at DESC, id
LIMIT sqlc.arg(max_rows);

-- name: StopTaskLoop :one
UPDATE task_loops SET state = 'stopped', updated_at = now(), lease_owner = NULL, lease_until = NULL
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND recipient_key = sqlc.arg(recipient_key)
  AND state = 'armed'
RETURNING *;

-- An evaluation's record, fenced on the claim, which it releases. The caller holds the row
-- (GetTaskLoopForUpdate) for the transaction.
-- name: SetTaskLoopEvaluation :execrows
UPDATE task_loops
SET state = sqlc.arg(state), baseline = sqlc.narg(baseline), fire_at = sqlc.narg(fire_at),
    firings = sqlc.arg(firings), last_fired_at = sqlc.narg(last_fired_at), quiet_since = sqlc.narg(quiet_since),
    progress_mark = sqlc.narg(progress_mark), unchanged_firings = sqlc.arg(unchanged_firings),
    stop_reason = sqlc.narg(stop_reason), stopped_version = sqlc.narg(stopped_version), updated_at = now(),
    next_attempt_at = sqlc.arg(next_attempt_at), lease_owner = NULL, lease_until = NULL
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts);

-- The loop's firing that is still waiting to be sent, if any: occurrences fold into it. Its delivery
-- row is locked, so a batch that is preparing it now either has taken it already (and this finds
-- nothing) or waits for the fold to commit.
-- name: GetPendingLoopFiring :one
SELECT f.* FROM task_loop_firings f
JOIN task_event_deliveries d ON d.loop_firing_id = f.id
WHERE f.loop_id = $1 AND d.state = 'pending' AND d.delivery_id IS NULL
ORDER BY f.number DESC
LIMIT 1
FOR UPDATE OF d, f;

-- name: FoldLoopFiring :exec
UPDATE task_loop_firings SET facts = sqlc.arg(facts)::text[], folded = sqlc.arg(folded) WHERE id = sqlc.arg(id);

-- name: InsertLoopFiring :one
INSERT INTO task_loop_firings (id, loop_id, number, facts, not_before, stops_loop)
VALUES (sqlc.arg(id), sqlc.arg(loop_id), sqlc.arg(number), sqlc.arg(facts)::text[], sqlc.arg(not_before), sqlc.narg(stops_loop))
RETURNING *;

-- name: InsertLoopFiringDelivery :exec
INSERT INTO task_event_deliveries (id, loop_firing_id, recipient_key) VALUES ($1, $2, $3);

-- name: GetLoopFiring :one
SELECT * FROM task_loop_firings WHERE id = $1;

-- The latest firings of a loop, newest first.
-- name: ListLoopFirings :many
SELECT f.*, d.state AS delivery_state FROM task_loop_firings f
JOIN task_event_deliveries d ON d.loop_firing_id = f.id
WHERE f.loop_id = sqlc.arg(loop_id)
ORDER BY f.number DESC
LIMIT sqlc.arg(max_rows);

-- Whether a task's supervisor is waiting on a loop: one is armed for it, or a firing to it is still
-- on its way. Its quiet is not a stall (internal/tasks/liveness.go).
-- name: TaskAwaitsLoop :one
SELECT (
    EXISTS (
        SELECT 1 FROM task_loops
        WHERE recipient_key = 'parent-task:' || sqlc.arg(task_id)::text AND state = 'armed'
          AND expires_at > sqlc.arg(now)::timestamptz
    )
    OR EXISTS (
        SELECT 1 FROM task_event_deliveries
        WHERE recipient_key = 'parent-task:' || sqlc.arg(task_id)::text AND loop_firing_id IS NOT NULL
          AND state IN ('pending', 'queued')
    )
)::boolean AS awaits;
