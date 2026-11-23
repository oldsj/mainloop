-- name: StartWebFetch :one
-- Records a fetch about to be made, unless the owner has made per_minute or more in the last
-- minute: then nothing is inserted and no row is returned.
INSERT INTO web_fetches (id, owner_id, session_id, url)
SELECT sqlc.arg(id)::text, sqlc.arg(owner_id)::text, sqlc.narg(session_id)::text, sqlc.arg(url)::text
WHERE (
    SELECT count(*) FROM web_fetches
    WHERE owner_id = sqlc.arg(owner_id)::text AND created_at > now() - interval '1 minute'
) < sqlc.arg(per_minute)::integer
RETURNING id;

-- name: FinishWebFetch :exec
UPDATE web_fetches
SET final_url = sqlc.narg(final_url), status = sqlc.narg(status), bytes = sqlc.narg(bytes),
    error_code = sqlc.narg(error_code), finished_at = now()
WHERE id = sqlc.arg(id);

-- name: SetBindingWebFetch :one
-- Sets a session's own web_fetch flag, and reads whether its task carries one too.
UPDATE agent_bindings AS b
SET web_fetch = sqlc.arg(web_fetch), updated_at = now()
FROM sessions AS s
WHERE b.session_id = sqlc.arg(session_id) AND s.id = b.session_id AND s.user_id = sqlc.arg(user_id)
RETURNING b.session_id, b.web_fetch,
    EXISTS (
        SELECT 1 FROM task_attempts AS a JOIN tasks AS t ON t.id = a.task_id
        WHERE a.binding_id = b.session_id AND t.web_fetch
    )::boolean AS from_task;

-- name: GetBindingWebFetch :one
SELECT b.session_id, b.web_fetch,
    EXISTS (
        SELECT 1 FROM task_attempts AS a JOIN tasks AS t ON t.id = a.task_id
        WHERE a.binding_id = b.session_id AND t.web_fetch
    )::boolean AS from_task
FROM agent_bindings AS b
WHERE b.session_id = $1;
