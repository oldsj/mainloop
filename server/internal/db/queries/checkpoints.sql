-- session_checkpoints and checkpoint_forks: durable intents for kagent's CheckpointService
-- (server/internal/sessions/checkpoints.go), following the reconciler rule (AGENTS.md).

-- name: GetSessionCheckpoint :one
SELECT * FROM session_checkpoints WHERE id = $1;

-- name: InsertSessionCheckpoint :one
INSERT INTO session_checkpoints (id, session_id, kagent_session_id, expected_head_task_id)
VALUES (sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(kagent_session_id), sqlc.arg(expected_head_task_id))
RETURNING *;

-- Records one unconfirmed attempt. updated_at moves, so the resume sweep waits before it looks
-- again.
-- name: NoteSessionCheckpointAttempt :exec
UPDATE session_checkpoints SET attempts = attempts + 1, detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending';

-- Zero rows when another caller already settled it.
-- name: FinishSessionCheckpointDone :execrows
UPDATE session_checkpoints
SET state = 'done', checkpoint_id = sqlc.narg(checkpoint_id), checkpoint_name = sqlc.narg(checkpoint_name),
    failure_reason = sqlc.narg(failure_reason), detail = sqlc.narg(detail), confirmed_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending';

-- Pending checkpoints nobody has touched for a while: a request that was cut off, or a kagent call
-- whose outcome was uncertain.
-- name: ListDueSessionCheckpoints :many
SELECT * FROM session_checkpoints
WHERE state = 'pending' AND updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
ORDER BY created_at, id
LIMIT sqlc.arg(limit_count);

-- name: GetCheckpointFork :one
SELECT * FROM checkpoint_forks WHERE id = $1;

-- name: InsertCheckpointFork :one
INSERT INTO checkpoint_forks (id, checkpoint_id, source_session_id, fresh_conversation, title)
VALUES (sqlc.arg(id), sqlc.arg(checkpoint_id), sqlc.arg(source_session_id), sqlc.arg(fresh_conversation), sqlc.arg(title))
RETURNING *;

-- name: NoteCheckpointForkAttempt :exec
UPDATE checkpoint_forks SET attempts = attempts + 1, detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND state IN ('requested', 'accepted');

-- kagent's ForkSession confirmed and the new session and binding were created in the same
-- transaction as this write.
-- name: FinishCheckpointForkAccepted :execrows
UPDATE checkpoint_forks
SET state = 'accepted', forked_kagent_session_id = sqlc.arg(forked_kagent_session_id),
    new_session_id = sqlc.arg(new_session_id), accepted_at = now(), detail = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested';

-- The forked runtime was observed READY.
-- name: FinishCheckpointForkReady :execrows
UPDATE checkpoint_forks SET state = 'ready', ready_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'accepted';

-- A definite kagent refusal: never retried.
-- name: FinishCheckpointForkFailed :execrows
UPDATE checkpoint_forks
SET state = 'failed', failure_reason = sqlc.arg(failure_reason), detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'requested';

-- Open forks nobody has touched for a while: requested (kagent's ForkSession outcome was
-- uncertain) or accepted (the forked runtime was not yet observed ready).
-- name: ListDueCheckpointForks :many
SELECT * FROM checkpoint_forks
WHERE state IN ('requested', 'accepted') AND updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
ORDER BY created_at, id
LIMIT sqlc.arg(limit_count);

-- The Mainloop session ids of forks that still pin a checkpoint: a DeleteCheckpoint that kagent
-- answered NotFound for, re-read and found still present (issue #311), names them in its 409.
-- name: ListForksByCheckpoint :many
SELECT new_session_id FROM checkpoint_forks
WHERE checkpoint_id = $1 AND new_session_id IS NOT NULL
ORDER BY accepted_at;
