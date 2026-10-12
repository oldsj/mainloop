-- Topic reads and writes the MCP tools need. The row CRUD is in core.sql.

-- name: ListTopicsWithPending :many
SELECT
    t.id,
    t.name,
    t.status_line,
    (
        SELECT count(*) FROM topic_records AS r
        WHERE r.topic_id = t.id AND r.kind = 'pending' AND r.status = 'open'
    )::int AS pending
FROM topics AS t
WHERE t.user_id = $1
ORDER BY t.updated_at DESC, t.id;

-- name: UpsertTopic :one
INSERT INTO topics (id, user_id, name) VALUES ($1, $2, $3)
ON CONFLICT (user_id, name) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: EnsureTopic :one
-- Returns the topic, creating it when missing. An existing topic keeps its updated_at; the no-op
-- update is what makes RETURNING yield the row under concurrent inserts.
INSERT INTO topics (id, user_id, name) VALUES ($1, $2, $3)
ON CONFLICT (user_id, name) DO UPDATE SET name = topics.name
RETURNING *;

-- name: SetTopicStatusLine :exec
UPDATE topics SET status_line = $2, updated_at = now() WHERE id = $1;

-- name: ClosePendingByPrefix :many
-- Closes open pending items of one owner whose id starts with the prefix. The caller refuses a
-- prefix that matched more than one row by rolling back, so a short prefix cannot close several.
UPDATE topic_records AS r
SET status = 'done'
FROM topics AS t
WHERE r.topic_id = t.id AND t.user_id = sqlc.arg(user_id) AND r.kind = 'pending' AND r.status = 'open'
    AND starts_with(r.id, sqlc.arg(prefix)::text)
RETURNING r.id;
