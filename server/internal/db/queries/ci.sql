-- name: InsertCheckWatch :one
INSERT INTO check_watches (id, idempotency_key, user_id, project_id, session_id, pr_number, commit_sha, deadline)
VALUES (
    sqlc.arg(id), sqlc.arg(idempotency_key), sqlc.arg(user_id), sqlc.arg(project_id), sqlc.arg(session_id),
    sqlc.narg(pr_number), sqlc.narg(commit_sha), sqlc.arg(deadline)
)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- name: GetCheckWatchByKey :one
SELECT * FROM check_watches WHERE idempotency_key = $1;

-- name: GetCheckWatch :one
SELECT * FROM check_watches WHERE id = $1;

-- A new watch of the same session and ref replaces the ones still polling: one message per ref.
-- Their leases are cleared, so a claimer that held one loses its fence.
-- name: SupersedeCheckWatches :execrows
UPDATE check_watches
SET state = 'superseded', lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE session_id = sqlc.arg(session_id) AND state = 'watching' AND id <> sqlc.arg(id)
  AND pr_number IS NOT DISTINCT FROM sqlc.narg(pr_number)
  AND commit_sha IS NOT DISTINCT FROM sqlc.narg(commit_sha);

-- Records the outcome and its message, keeping the lease so the same claim delivers it.
-- name: SettleCheckWatch :execrows
UPDATE check_watches
SET state = 'delivering', outcome = sqlc.arg(outcome), head_sha = sqlc.narg(head_sha),
    message = sqlc.arg(message), updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts)
  AND state = 'watching';

-- Ends a watch in a final state, releasing its lease.
-- name: FinishCheckWatch :execrows
UPDATE check_watches
SET state = sqlc.arg(state), detail = sqlc.narg(detail), lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND lease_owner = sqlc.arg(lease_owner) AND attempts = sqlc.arg(attempts);

-- name: GetProjectVerification :one
SELECT id, full_name, default_branch, verification_mode, ci_detection, ci_detected_at
FROM projects
WHERE id = $1 AND user_id = $2;

-- name: SetProjectVerificationMode :execrows
UPDATE projects SET verification_mode = $3 WHERE id = $1 AND user_id = $2;

-- name: SetProjectCIDetection :execrows
UPDATE projects SET ci_detection = $3, ci_detected_at = $4 WHERE id = $1 AND user_id = $2;
