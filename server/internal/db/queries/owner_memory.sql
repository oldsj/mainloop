-- Owner memory (docs/specs/owner-memory.md). Writers hold the owner's settings row lock
-- (LockOwnerMemory), so the sequence and the active byte total are read and advanced by one writer
-- at a time.

-- name: EnsureOwnerMemorySettings :exec
INSERT INTO owner_memory_settings (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING;

-- name: LockOwnerMemory :one
SELECT * FROM owner_memory_settings WHERE user_id = $1 FOR UPDATE;

-- The owner's settings, or the defaults for an owner who has none yet.
-- name: GetOwnerMemorySettings :one
SELECT coalesce((SELECT s.review_after_days FROM owner_memory_settings s WHERE s.user_id = $1), 14)::integer AS review_after_days,
       coalesce((SELECT s.seq FROM owner_memory_settings s WHERE s.user_id = $1), 0)::bigint AS seq;

-- name: SetOwnerMemorySeq :exec
UPDATE owner_memory_settings SET seq = sqlc.arg(seq)::bigint, updated_at = now() WHERE user_id = sqlc.arg(user_id);

-- name: SetOwnerMemoryReviewDays :one
INSERT INTO owner_memory_settings (user_id, review_after_days) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET review_after_days = EXCLUDED.review_after_days, updated_at = now()
RETURNING *;

-- name: ActiveOwnerMemoryBytes :one
SELECT coalesce(sum(octet_length(text)), 0)::bigint
FROM owner_memory_entries WHERE user_id = $1 AND state = 'active';

-- name: CountProposedOwnerMemory :one
SELECT count(*) FROM owner_memory_entries WHERE user_id = $1 AND state = 'proposed';

-- name: InsertOwnerMemoryEntry :one
INSERT INTO owner_memory_entries (
    id, user_id, text, state, author, source_session_id, source_task_id, source_attempt_id,
    source_delivery_id, replaces_id, last_confirmed_at, decided_at
) VALUES (
    sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(text), sqlc.arg(state), sqlc.arg(author),
    sqlc.narg(source_session_id), sqlc.narg(source_task_id), sqlc.narg(source_attempt_id),
    sqlc.narg(source_delivery_id), sqlc.narg(replaces_id), sqlc.narg(last_confirmed_at),
    sqlc.narg(last_confirmed_at)
) RETURNING *;

-- name: GetOwnerMemoryEntry :one
SELECT * FROM owner_memory_entries WHERE id = $1 AND user_id = $2;

-- An open entry with this exact text: a repeated remember answers it instead of adding another.
-- name: FindOpenOwnerMemoryByText :one
SELECT * FROM owner_memory_entries
WHERE user_id = $1 AND text = $2 AND state IN ('active', 'lapsed', 'proposed')
ORDER BY created_at, id
LIMIT 1;

-- Active entries, then lapsed ones, then proposals, each oldest first.
-- name: ListOpenOwnerMemory :many
SELECT * FROM owner_memory_entries
WHERE user_id = $1 AND state IN ('active', 'lapsed', 'proposed')
ORDER BY CASE state WHEN 'active' THEN 0 WHEN 'lapsed' THEN 1 ELSE 2 END, created_at, id;

-- The entries active at a sequence, with the confirmation each had then, oldest first.
-- name: ListOwnerMemoryAt :many
SELECT e.id, e.text, s.confirmed_at
FROM owner_memory_spans s
JOIN owner_memory_entries e ON e.id = s.entry_id
WHERE s.user_id = sqlc.arg(user_id)
  AND s.start_seq <= sqlc.arg(seq)::bigint AND (s.end_seq IS NULL OR s.end_seq > sqlc.arg(seq)::bigint)
ORDER BY e.created_at, e.id;

-- name: StartOwnerMemorySpan :exec
INSERT INTO owner_memory_spans (entry_id, user_id, start_seq, confirmed_at)
VALUES (sqlc.arg(entry_id), sqlc.arg(user_id), sqlc.arg(seq)::bigint, sqlc.arg(confirmed_at)::timestamptz);

-- name: EndOwnerMemorySpan :exec
UPDATE owner_memory_spans SET end_seq = sqlc.arg(seq)::bigint
WHERE entry_id = sqlc.arg(entry_id) AND end_seq IS NULL;

-- Makes a proposal or a lapsed entry active, confirmed at confirmed_at.
-- name: ActivateOwnerMemoryEntry :one
UPDATE owner_memory_entries
SET state = 'active', last_confirmed_at = sqlc.arg(confirmed_at)::timestamptz, lapsed_at = NULL, decided_at = sqlc.arg(confirmed_at)::timestamptz
WHERE id = sqlc.arg(id) AND state IN ('proposed', 'lapsed')
RETURNING *;

-- name: ConfirmOwnerMemoryEntry :one
UPDATE owner_memory_entries SET last_confirmed_at = sqlc.arg(confirmed_at)::timestamptz
WHERE id = sqlc.arg(id) AND state = 'active'
RETURNING *;

-- Ends an active or lapsed entry as replaced, forgotten or moved, or lapses an active one.
-- name: EndOwnerMemoryEntry :one
UPDATE owner_memory_entries
SET state = sqlc.arg(state), decided_at = sqlc.arg(at)::timestamptz,
    lapsed_at = CASE WHEN sqlc.arg(state)::text = 'lapsed' THEN sqlc.arg(at)::timestamptz ELSE lapsed_at END,
    moved_to_project_id = sqlc.narg(moved_to_project_id), moved_to_entry_id = sqlc.narg(moved_to_entry_id)
WHERE id = sqlc.arg(id) AND state IN ('active', 'lapsed')
RETURNING *;

-- Closes a proposal as rejected or withdrawn.
-- name: CloseOwnerMemoryProposal :one
UPDATE owner_memory_entries SET state = sqlc.arg(state), decided_at = now()
WHERE id = sqlc.arg(id) AND state = 'proposed'
RETURNING *;

-- Active entries whose review window and grace have both run out by now.
-- name: ListLapsingOwnerMemory :many
SELECT e.* FROM owner_memory_entries e
LEFT JOIN owner_memory_settings s ON s.user_id = e.user_id
WHERE e.user_id = sqlc.arg(user_id) AND e.state = 'active'
  AND e.last_confirmed_at + make_interval(days => coalesce(s.review_after_days, 14) + sqlc.arg(grace_days)::integer) <= sqlc.arg(now)::timestamptz
ORDER BY e.created_at, e.id;

-- name: ListOwnerMemoryForDotfiles :many
SELECT * FROM owner_memory_entries WHERE user_id = $1 AND state IN ('active', 'lapsed') ORDER BY created_at, id;

-- name: SetOwnerMemoryInDotfiles :exec
UPDATE owner_memory_entries SET in_dotfiles_commit = sqlc.narg(commit)
WHERE id = sqlc.arg(id) AND in_dotfiles_commit IS DISTINCT FROM sqlc.narg(commit);

-- The pinned owner memory sequence of an attempt's brief, pinned once at the owner's current one.
-- name: PinAttemptOwnerMemorySeq :one
UPDATE task_attempts SET owner_memory_seq = coalesce(owner_memory_seq, (
    SELECT s.seq FROM owner_memory_settings s WHERE s.user_id = sqlc.arg(user_id)), 0)
WHERE task_attempts.id = sqlc.arg(id) RETURNING owner_memory_seq;

-- What owner memory a session has (docs/specs/owner-memory.md#where-it-loads): what the newest
-- other delivery that gave it some gave, unless kagent never saw that message (failed or cancelled
-- with no task), else what its newest attempt's brief pinned. NULL: it was given none.
-- name: OwnerMemoryGiven :one
WITH g AS (
    SELECT coalesce((
        SELECT d.given_owner_memory_seq FROM deliveries d
        WHERE d.session_id = sqlc.arg(session_id)::text AND d.id <> sqlc.arg(delivery_id)::text
          AND d.given_owner_memory_seq IS NOT NULL
          AND NOT (d.state IN ('failed', 'cancelled') AND d.task_id IS NULL)
        ORDER BY d.created_at DESC, d.id DESC
        LIMIT 1
    ), (
        SELECT a.owner_memory_seq FROM task_attempts a
        WHERE a.session_id = sqlc.arg(session_id)::text
        ORDER BY a.number DESC
        LIMIT 1
    )) AS seq
)
SELECT (g.seq IS NOT NULL)::boolean AS given, coalesce(g.seq, 0)::bigint AS seq FROM g;

-- One 'review' item per owner while any owner memory proposal waits, or any entry is due, lapsed
-- or already in the dotfiles. Its line counts both; a changed count rewrites it.
-- name: OpenOwnerMemoryAttention :many
INSERT INTO attention_items AS a (id, owner_id, kind, source_kind, source_id, title, needed, href)
SELECT 'attn_owner_memory_' || n.user_id, n.user_id, 'review', 'owner_memory', n.user_id, 'Owner memory',
       concat_ws(' and ',
           CASE WHEN n.proposed = 1 THEN '1 owner memory proposal'
                WHEN n.proposed > 1 THEN n.proposed::text || ' owner memory proposals' END,
           CASE WHEN n.review = 1 AND n.proposed > 0 THEN '1 entry to review'
                WHEN n.review > 1 AND n.proposed > 0 THEN n.review::text || ' entries to review'
                WHEN n.review = 1 THEN '1 owner memory entry to review'
                WHEN n.review > 1 THEN n.review::text || ' owner memory entries to review' END) || '.',
       '/settings#memory'
FROM (
    SELECT sqlc.arg(user_id)::text AS user_id,
           count(*) FILTER (WHERE e.state = 'proposed')::int AS proposed,
           count(*) FILTER (WHERE e.state = 'lapsed' OR (e.state = 'active' AND (
               e.in_dotfiles_commit IS NOT NULL
               OR e.last_confirmed_at + make_interval(days => coalesce(s.review_after_days, 14)) <= sqlc.arg(now)::timestamptz
           )))::int AS review
    FROM owner_memory_entries e
    LEFT JOIN owner_memory_settings s ON s.user_id = e.user_id
    WHERE e.user_id = sqlc.arg(user_id)::text AND e.state IN ('proposed', 'active', 'lapsed')
) n
WHERE n.proposed + n.review > 0
ON CONFLICT (source_kind, source_id) DO UPDATE
SET state = 'open', resolved_at = NULL, updated_at = now(), title = EXCLUDED.title, needed = EXCLUDED.needed,
    read_at = CASE WHEN a.state <> 'open' THEN NULL ELSE a.read_at END
WHERE a.state <> 'open' OR a.title <> EXCLUDED.title OR a.needed <> EXCLUDED.needed
RETURNING id;

-- The owner memory item resolves once nothing waits: the exact negation of the open condition.
-- name: ResolveOwnerMemoryAttention :many
UPDATE attention_items a SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE a.owner_id = sqlc.arg(user_id)::text AND a.source_kind = 'owner_memory' AND a.state = 'open'
  AND NOT EXISTS (
      SELECT 1 FROM owner_memory_entries e
      LEFT JOIN owner_memory_settings s ON s.user_id = e.user_id
      WHERE e.user_id = a.owner_id AND (
          e.state = 'proposed' OR e.state = 'lapsed' OR (e.state = 'active' AND (
              e.in_dotfiles_commit IS NOT NULL
              OR e.last_confirmed_at + make_interval(days => coalesce(s.review_after_days, 14)) <= sqlc.arg(now)::timestamptz))))
RETURNING a.id;
