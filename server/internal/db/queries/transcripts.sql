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
-- Matches are marked with U+0002 and U+0003, not HTML: the text is not escaped, and the API splits
-- the snippet at the marks.
SELECT e.id, e.session_id, s.title AS session_title, e.task_id, e.pos, e.kind, e.tool_name, e.at,
       ts_headline('english', e.text, q,
           'MaxFragments=2, MaxWords=24, MinWords=8, FragmentDelimiter=" … ", StartSel=' || chr(2) || ', StopSel=' || chr(3))::text AS snippet,
       ts_rank(e.tsv, q)::real AS rank
FROM transcript_entries e
JOIN sessions s ON s.id = e.session_id,
     websearch_to_tsquery('english', sqlc.arg(query)::text) AS q
WHERE s.user_id = sqlc.arg(owner_id) AND e.tsv @@ q
  AND (sqlc.narg(session_id)::text IS NULL OR e.session_id = sqlc.narg(session_id))
  AND (sqlc.narg(project_id)::text IS NULL OR s.project_id = sqlc.narg(project_id))
  AND (sqlc.narg(session_ids)::text[] IS NULL OR e.session_id = ANY(sqlc.narg(session_ids)::text[]))
ORDER BY rank DESC, e.id DESC
LIMIT sqlc.arg(max_results);

-- name: ListTranscriptBindingsToAdopt :many
-- The owner's bound kagent Sessions the ingester does not follow yet, with what the sessions package
-- needs to resolve the Agent that runs each. Archived sessions are included: their last turns are
-- still copied.
SELECT b.session_id, b.kagent_session_id, s.user_id, b.kind, b.role,
       coalesce(b.kagent_agent_namespace, w.agent_namespace, '') AS resolved_agent_namespace,
       coalesce(b.kagent_agent_name, w.agent_name, '') AS resolved_agent_name
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
LEFT JOIN workspaces w ON w.id = b.session_id
WHERE b.kagent_session_id IS NOT NULL AND s.user_id = $1
  AND NOT EXISTS (SELECT 1 FROM transcript_sessions t WHERE t.kagent_session_id = b.kagent_session_id)
ORDER BY b.session_id
LIMIT sqlc.arg(max_rows);

-- name: ListDueTranscriptSessions :many
SELECT * FROM transcript_sessions
WHERE owner_id = $1 AND state = 'active' AND next_scan_at <= now()
ORDER BY next_scan_at, kagent_session_id
LIMIT sqlc.arg(max_sessions);

-- name: SetTranscriptSessionScan :exec
-- A pass that read the session without failing: the cursor moves (or stays), the failure backoff
-- clears and the session rests until it is due again or a finished turn nudges it.
UPDATE transcript_sessions
SET task_cursor = sqlc.narg(cursor), scanned_at = now(), failures = 0, updated_at = now(),
    next_scan_at = now() + make_interval(secs => sqlc.arg(rest_seconds)::float8)
WHERE kagent_session_id = sqlc.arg(kagent_session_id);

-- name: FailTranscriptSessionScan :exec
UPDATE transcript_sessions
SET failures = failures + 1, updated_at = now(),
    next_scan_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8)
WHERE kagent_session_id = sqlc.arg(kagent_session_id);

-- name: EndTranscriptSession :exec
-- The session is no longer followed: complete (reason empty) or unavailable (reason says why).
UPDATE transcript_sessions SET state = $2, reason = $3, updated_at = now()
WHERE kagent_session_id = $1 AND state = 'active';

-- name: NudgeTranscriptSession :exec
-- A turn of this kagent Session just ended: copy it on the next pass.
UPDATE transcript_sessions SET next_scan_at = now()
WHERE kagent_session_id = $1 AND state = 'active' AND next_scan_at > now();

-- name: CopiedTranscriptTasks :many
-- Which of these tasks have a copy receipt.
SELECT task_id FROM transcript_tasks WHERE task_id = ANY(sqlc.arg(task_ids)::text[]);

-- name: CountUncopiedDeliveries :one
-- Turns Mainloop sent into this kagent Session that have no copy: a Session that is gone with any
-- of these lost them.
SELECT count(*) FROM deliveries d
WHERE d.kagent_session_id = $1 AND d.task_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM transcript_tasks t WHERE t.task_id = d.task_id);

-- name: TranscriptSessionBytes :one
SELECT stored_bytes FROM transcript_sessions WHERE kagent_session_id = $1;

-- name: PruneTranscriptTasks :execrows
-- Retention: the entries of up to max_rows tasks copied before the cutoff are deleted, and their
-- receipts kept as pruned so a task kagent still lists is not copied again.
WITH old AS (
    SELECT tt.task_id FROM transcript_tasks tt
    WHERE tt.pruned_at IS NULL AND tt.ingested_at < sqlc.arg(cutoff)
    ORDER BY tt.ingested_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
), gone AS (
    DELETE FROM transcript_entries e USING old WHERE e.task_id = old.task_id
)
UPDATE transcript_tasks t SET pruned_at = now() FROM old WHERE t.task_id = old.task_id;

-- name: GetTranscriptSession :one
SELECT * FROM transcript_sessions WHERE kagent_session_id = $1;

-- name: TranscriptSessionOwned :one
SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND user_id = $2);

-- name: ListTranscriptEntries :many
-- A session's stored entries after an id, in the order they were copied, of the given kinds (all
-- when null).
SELECT id, task_id, pos, kind, tool_name, tool_call_id, is_error, text, truncated, orig_bytes, at
FROM transcript_entries
WHERE session_id = $1 AND id > sqlc.arg(after_id)
  AND (sqlc.narg(kinds)::text[] IS NULL OR kind = ANY(sqlc.narg(kinds)::text[]))
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: ListTranscriptEntriesBefore :many
-- A session's stored entries before an id (the last ones when null), newest first, of the given
-- kinds (all when null).
SELECT id, task_id, pos, kind, tool_name, tool_call_id, is_error, text, truncated, orig_bytes, at
FROM transcript_entries
WHERE session_id = $1 AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
  AND (sqlc.narg(kinds)::text[] IS NULL OR kind = ANY(sqlc.narg(kinds)::text[]))
ORDER BY id DESC
LIMIT sqlc.arg(max_rows);

-- name: TranscriptEntriesBeyond :one
-- Whether a session has stored entries of the given kinds before lo and after hi.
SELECT
  EXISTS (SELECT 1 FROM transcript_entries e WHERE e.session_id = $1 AND e.id < sqlc.arg(lo)::bigint
          AND (sqlc.narg(kinds)::text[] IS NULL OR e.kind = ANY(sqlc.narg(kinds)::text[]))) AS earlier,
  EXISTS (SELECT 1 FROM transcript_entries e WHERE e.session_id = $1 AND e.id > sqlc.arg(hi)::bigint
          AND (sqlc.narg(kinds)::text[] IS NULL OR e.kind = ANY(sqlc.narg(kinds)::text[]))) AS later;

-- name: ListTranscriptSources :many
SELECT * FROM transcript_sessions WHERE session_id = $1 ORDER BY created_at DESC, kagent_session_id LIMIT 50;

-- name: GetTranscriptEntry :one
SELECT id, session_id, task_id, pos, kind, tool_name, tool_call_id, is_error, text, truncated, orig_bytes, at
FROM transcript_entries WHERE id = $1;
