-- Preview screenshots (docs/specs/workspaces.md "Preview screenshots").

-- name: CountRecentScreenshotAttempts :one
-- The captures a session tried in the last minute, stored or not, for the per-session rate limit.
SELECT coalesce(sum(captures), 0)::integer FROM preview_screenshot_attempts
WHERE session_id = $1 AND created_at > now() - interval '1 minute';

-- name: InsertScreenshotAttempt :exec
INSERT INTO preview_screenshot_attempts (session_id, captures) VALUES ($1, $2);

-- name: PruneScreenshotAttempts :execrows
-- Attempts older than the limit's window count for nothing.
DELETE FROM preview_screenshot_attempts WHERE created_at < now() - interval '1 hour';

-- name: ExpireScreenshotImages :execrows
-- Drops a screenshot's image, keeping the row, when it is older than max_age; when it is one of a
-- workspace's screenshots that no report names beyond its newest keep; or when the workspace's
-- held images pass max_bytes. The byte budget counts reported images too and spends itself on
-- them first, newest first, then on the unreported ones, newest first. The two caps leave images
-- younger than grace alone and do not count them, so an agent can report what it just captured.
WITH reported AS (
    SELECT DISTINCT u.id FROM task_reports r CROSS JOIN LATERAL unnest(r.screenshot_ids) AS u(id)
),
held AS (
    SELECT s.id, s.reported,
           row_number() OVER (PARTITION BY s.session_id, s.reported ORDER BY s.created_at DESC, s.id DESC) AS nth,
           sum(s.size_bytes) OVER (PARTITION BY s.session_id
               ORDER BY s.reported DESC, s.created_at DESC, s.id DESC) AS bytes
    FROM (
        SELECT p.id, p.session_id, p.created_at, p.size_bytes, (r.id IS NOT NULL) AS reported
        FROM preview_screenshots p LEFT JOIN reported r ON r.id = p.id
        WHERE p.expired_at IS NULL
          AND p.created_at >= now() - make_interval(secs => sqlc.arg(max_age_seconds)::float8)
          AND p.created_at < now() - make_interval(secs => sqlc.arg(grace_seconds)::float8)
    ) s
)
UPDATE preview_screenshots s SET data = NULL, expired_at = now()
WHERE s.expired_at IS NULL AND (
    s.created_at < now() - make_interval(secs => sqlc.arg(max_age_seconds)::float8)
    OR s.id IN (
        SELECT h.id FROM held h
        WHERE (NOT h.reported AND h.nth > sqlc.arg(keep)::integer) OR h.bytes > sqlc.arg(max_bytes)::bigint
    )
);

-- name: PruneScreenshotCopies :execrows
-- Deletes the chat copies of report screenshots on the screenshots' schedule: older than max_age,
-- copied from a screenshot whose image expired, or past max_bytes of copies in one conversation
-- (oldest first). A copy younger than grace is left alone, so a send in flight still finds the
-- images it copied; past that, a copy no message claimed is an orphan of a failed send.
WITH ranked AS (
    SELECT m.id, sum(m.size_bytes) OVER (PARTITION BY m.session_id ORDER BY m.created_at DESC, m.id DESC) AS bytes
    FROM message_images m
    WHERE m.screenshot_id IS NOT NULL AND m.message_id IS NOT NULL
)
DELETE FROM message_images m
WHERE m.screenshot_id IS NOT NULL AND m.created_at < now() - make_interval(secs => sqlc.arg(grace_seconds)::float8) AND (
    m.message_id IS NULL
    OR m.created_at < now() - make_interval(secs => sqlc.arg(max_age_seconds)::float8)
    OR EXISTS (SELECT 1 FROM preview_screenshots s WHERE s.id = m.screenshot_id AND s.expired_at IS NOT NULL)
    OR m.id IN (SELECT r.id FROM ranked r WHERE r.bytes > sqlc.arg(max_bytes)::bigint)
);

-- name: ListPreviewScreenshotsByRequest :many
-- What a request already stored, so a retry returns it instead of capturing again.
SELECT * FROM preview_screenshots
WHERE session_id = $1 AND request_id = $2
ORDER BY viewport;

-- name: InsertPreviewScreenshot :exec
INSERT INTO preview_screenshots (
    id, owner_id, session_id, request_id, port, path, viewport, full_page, media_type, width, height,
    page_status, data, size_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (session_id, request_id, viewport) DO NOTHING;

-- name: CountAttemptScreenshots :one
-- How many of ids are the owner's screenshots of the attempt's own workspace: a report may show
-- only those.
SELECT count(*) FROM preview_screenshots s
JOIN task_attempts a ON a.id = sqlc.arg(attempt_id)::text
WHERE s.id = ANY(sqlc.arg(ids)::text[]) AND s.owner_id = sqlc.arg(owner_id)::text
  AND s.session_id IN (a.binding_id, a.session_id, a.workspace_id);

-- name: GetTaskReportScreenshot :one
-- One screenshot with its bytes, when a report of the owner's task shows it.
SELECT s.* FROM preview_screenshots s
WHERE s.id = sqlc.arg(id)::text AND s.owner_id = sqlc.arg(owner_id)::text
  AND EXISTS (
    SELECT 1 FROM task_reports r JOIN tasks t ON t.id = r.task_id
    WHERE r.task_id = sqlc.arg(task_id)::text AND t.owner_id = sqlc.arg(owner_id)::text
      AND s.id = ANY(r.screenshot_ids)
  );

-- name: CopyReportScreenshotsToMessage :exec
-- Copies the screenshots of the reports a message delivers into chat images of the recipient's
-- conversation, unclaimed until the send attaches them. Ids derive from the message, so a retry of
-- the same message writes nothing new.
INSERT INTO message_images (id, session_id, message_id, media_type, data, size_bytes, screenshot_id)
SELECT sqlc.arg(message_id)::text || ':' || s.id, sqlc.arg(session_id)::text, NULL, s.media_type, s.data, s.size_bytes, s.id
FROM task_reports r
CROSS JOIN LATERAL unnest(r.screenshot_ids) AS u(id)
JOIN preview_screenshots s ON s.id = u.id
WHERE r.id = ANY(sqlc.arg(report_ids)::text[]) AND s.data IS NOT NULL
ON CONFLICT (id) DO NOTHING;

-- name: ListMessageImageIDs :many
-- Which of ids a session holds as chat images: the report screenshots that were copied (an expired
-- one is not).
SELECT id FROM message_images WHERE session_id = sqlc.arg(session_id)::text AND id = ANY(sqlc.arg(ids)::text[]);
