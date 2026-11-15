-- Session transcripts (internal/transcripts, docs/specs/transcripts.md). Every text written here was
-- redacted and bounded by the caller first.

-- name: InsertTranscriptTask :execrows
-- The copy receipt of one task. Zero rows means the task was copied before; the caller then writes
-- no entries.
INSERT INTO transcript_tasks (task_id, kagent_session_id, session_id, task_state, entries, bytes, task_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (task_id) DO NOTHING;

-- name: InsertTranscriptEntries :exec
-- A task's entries, one array element each, in timeline order. The arrays have one length (set-
-- returning functions in one select list run in lockstep).
INSERT INTO transcript_entries (task_id, session_id, pos, kind, tool_name, tool_call_id, is_error, text, truncated, orig_bytes, at)
SELECT sqlc.arg(task_id)::text, sqlc.arg(session_id)::text,
       unnest(sqlc.arg(pos)::integer[]), unnest(sqlc.arg(kind)::text[]), unnest(sqlc.arg(tool_name)::text[]),
       unnest(sqlc.arg(tool_call_id)::text[]), unnest(sqlc.arg(is_error)::boolean[]), unnest(sqlc.arg(text)::text[]),
       unnest(sqlc.arg(truncated)::boolean[]), unnest(sqlc.arg(orig_bytes)::integer[]), nullif(unnest(sqlc.arg(at)::text[]), '')::timestamptz
ON CONFLICT (task_id, pos) DO NOTHING;

-- name: InsertTranscriptSession :exec
INSERT INTO transcript_sessions (kagent_session_id, session_id, owner_id, agent_namespace, agent_name, context_id)
VALUES ($1, $2, $3, $4, $5, $1)
ON CONFLICT (kagent_session_id) DO NOTHING;

-- name: AddTranscriptSessionBytes :exec
UPDATE transcript_sessions SET stored_bytes = stored_bytes + sqlc.arg(bytes)::bigint, updated_at = now()
WHERE kagent_session_id = sqlc.arg(kagent_session_id);

-- name: SearchTranscripts :many
-- The owner's entries that match a web-search style query, best first. The vector is built from the
-- stored text, which was redacted before it was written.
SELECT e.id, e.session_id, e.task_id, e.pos, e.kind, e.tool_name, e.at,
       ts_headline('english', e.text, q, 'MaxFragments=2, MaxWords=24, MinWords=8')::text AS snippet,
       ts_rank(e.tsv, q)::real AS rank
FROM transcript_entries e
JOIN sessions s ON s.id = e.session_id,
     websearch_to_tsquery('english', sqlc.arg(query)::text) AS q
WHERE s.user_id = sqlc.arg(owner_id) AND e.tsv @@ q
  AND (sqlc.narg(session_id)::text IS NULL OR e.session_id = sqlc.narg(session_id))
ORDER BY rank DESC, e.id DESC
LIMIT sqlc.arg(max_results);
