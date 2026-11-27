-- name: GetOwnerDotfilesSource :one
SELECT * FROM owner_dotfiles_sources WHERE owner_id = $1;

-- First write for an owner: only succeeds while no row exists (expected_version 0).
-- name: InsertOwnerDotfilesSource :one
INSERT INTO owner_dotfiles_sources (owner_id, repository, ref, path, version)
VALUES ($1, $2, $3, $4, 1)
ON CONFLICT (owner_id) DO NOTHING
RETURNING *;

-- Compare-and-swap on the version: no row back means the caller's version was stale.
-- name: UpdateOwnerDotfilesSource :one
UPDATE owner_dotfiles_sources
SET repository = $2, ref = $3, path = $4, version = version + 1, updated_at = now()
WHERE owner_id = $1 AND version = sqlc.arg(expected_version)
RETURNING *;

-- name: GetProjectDotfilesSource :one
SELECT * FROM project_dotfiles_sources WHERE project_id = $1;

-- name: InsertProjectDotfilesSource :one
INSERT INTO project_dotfiles_sources (project_id, repository, ref, path, version)
VALUES ($1, $2, $3, $4, 1)
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- name: UpdateProjectDotfilesSource :one
UPDATE project_dotfiles_sources
SET repository = $2, ref = $3, path = $4, version = version + 1, updated_at = now()
WHERE project_id = $1 AND version = sqlc.arg(expected_version)
RETURNING *;

-- name: GetDotfilesExport :one
SELECT * FROM dotfiles_exports WHERE repository = $1 AND commit = $2 AND path = $3 AND format = $4;

-- A concurrent export of the same commit wrote the same row; the second write changes nothing.
-- name: InsertDotfilesExport :exec
INSERT INTO dotfiles_exports (repository, commit, path, format, status, reason, digest, findings)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (repository, commit, path, format) DO NOTHING;

-- name: InsertDotfilesBundle :exec
INSERT INTO dotfiles_bundles (digest, content) VALUES ($1, $2)
ON CONFLICT (digest) DO NOTHING;

-- name: GetDotfilesBundle :one
SELECT content FROM dotfiles_bundles WHERE digest = $1;

-- name: GetSessionDotfilesSourceInputs :one
SELECT s.user_id, s.project_id FROM sessions s WHERE s.id = $1;

-- name: GetPinnedDotfiles :one
SELECT * FROM session_dotfiles WHERE request_id = $1;

-- The first pin of a request wins; a concurrent pin of the same request changes nothing.
-- name: InsertPinnedDotfiles :exec
INSERT INTO session_dotfiles (request_id, session_id, repository, ref, path, commit, digest, status, reason, url)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (request_id) DO NOTHING;

-- The pin of the session's newest native Session, or of its newest applied refresh: a refresh that
-- is pending or failed has not changed what the Session holds.
-- name: LatestSessionDotfiles :one
SELECT d.* FROM session_dotfiles d
WHERE d.session_id = $1 AND NOT EXISTS (
    SELECT 1 FROM session_dotfiles_refreshes r WHERE d.request_id = 'refresh:' || r.id AND r.state <> 'applied'
)
ORDER BY d.created_at DESC, d.request_id LIMIT 1;

-- A fork is given the files its source's Session was laid out with: its latest pin. kagent forks
-- the Session, so no create sends them and the url stays NULL.
-- name: CopyForkDotfiles :exec
INSERT INTO session_dotfiles (request_id, session_id, repository, ref, path, commit, digest, status, reason)
SELECT sqlc.arg(request_id), sqlc.arg(session_id), d.repository, d.ref, d.path, d.commit, d.digest, d.status, d.reason
FROM session_dotfiles d
WHERE d.session_id = sqlc.arg(source_session_id) AND NOT EXISTS (
    SELECT 1 FROM session_dotfiles_refreshes r WHERE d.request_id = 'refresh:' || r.id AND r.state <> 'applied'
)
ORDER BY d.created_at DESC, d.request_id
LIMIT 1
ON CONFLICT (request_id) DO NOTHING;

-- Whether the session was pinned to the bundle, by any of its native Sessions or its fork source.
-- name: SessionPinnedToDigest :one
SELECT EXISTS (SELECT 1 FROM session_dotfiles WHERE session_id = $1 AND digest = $2);

-- name: GetDotfilesRefresh :one
SELECT * FROM session_dotfiles_refreshes WHERE id = $1;

-- name: GetPendingDotfilesRefresh :one
SELECT * FROM session_dotfiles_refreshes WHERE session_id = $1 AND state = 'pending';

-- name: LatestDotfilesRefresh :one
SELECT * FROM session_dotfiles_refreshes WHERE session_id = $1 ORDER BY created_at DESC, id LIMIT 1;

-- name: InsertDotfilesRefresh :one
INSERT INTO session_dotfiles_refreshes (id, session_id, kagent_session_id)
VALUES ($1, $2, $3)
RETURNING *;

-- Records one attempt kagent did not answer. updated_at moves, so the resume sweep waits before it
-- looks again.
-- name: NoteDotfilesRefreshAttempt :exec
UPDATE session_dotfiles_refreshes SET attempts = attempts + 1, detail = sqlc.narg(detail), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending';

-- Settles a pending refresh; no row back when another caller settled it first.
-- name: SettleDotfilesRefresh :one
UPDATE session_dotfiles_refreshes SET state = sqlc.arg(state), reason = sqlc.arg(reason), detail = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending'
RETURNING *;

-- Pending refreshes nobody has touched for a while: a request that was cut off, or an attempt
-- kagent did not answer.
-- name: ListDueDotfilesRefreshes :many
SELECT * FROM session_dotfiles_refreshes
WHERE state = 'pending' AND updated_at < now() - make_interval(secs => sqlc.arg(idle_seconds)::float8)
ORDER BY created_at, id
LIMIT sqlc.arg(limit_count);
