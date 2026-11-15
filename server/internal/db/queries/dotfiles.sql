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
