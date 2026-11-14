-- name: GetOwnerSeen :one
SELECT * FROM owner_seen WHERE user_id = $1;

-- name: EnsureOwnerSeen :exec
INSERT INTO owner_seen (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING;

-- name: RecordVisit :one
-- The visit before this one becomes previous_visit, from whichever device made it.
UPDATE owner_seen SET previous_visit = last_visit, last_visit = now()
WHERE user_id = $1
RETURNING *;

-- name: ListTaskSeen :many
SELECT task_id, seen_at FROM task_seen
WHERE user_id = $1 AND seen_at > $2
ORDER BY seen_at DESC, task_id;

-- name: MarkTaskSeen :one
-- Only the owner's own tasks; a later mark wins, an earlier one never replaces it.
INSERT INTO task_seen (user_id, task_id, seen_at)
SELECT sqlc.arg(user_id), t.id, now() FROM tasks t WHERE t.id = sqlc.arg(task_id) AND t.owner_id = sqlc.arg(user_id)
ON CONFLICT (user_id, task_id) DO UPDATE SET seen_at = greatest(task_seen.seen_at, excluded.seen_at)
RETURNING task_id, seen_at;

-- name: PruneTaskSeen :exec
DELETE FROM task_seen WHERE user_id = $1 AND seen_at <= $2;
