-- Preview screenshots (docs/specs/workspaces.md "Preview screenshots").

-- name: CountRecentPreviewScreenshots :one
-- A session's captures in the last minute, for the per-session rate limit.
SELECT count(*) FROM preview_screenshots
WHERE session_id = $1 AND created_at > now() - interval '1 minute';

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
INSERT INTO message_images (id, session_id, message_id, media_type, data, size_bytes)
SELECT sqlc.arg(message_id)::text || ':' || s.id, sqlc.arg(session_id)::text, NULL, s.media_type, s.data, s.size_bytes
FROM task_reports r
CROSS JOIN LATERAL unnest(r.screenshot_ids) AS u(id)
JOIN preview_screenshots s ON s.id = u.id
WHERE r.id = ANY(sqlc.arg(report_ids)::text[])
ON CONFLICT (id) DO NOTHING;
