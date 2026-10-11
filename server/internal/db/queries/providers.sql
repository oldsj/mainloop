-- name: GetProviderPreference :one
SELECT * FROM project_provider_preferences WHERE project_id = $1;

-- First write for a project: only succeeds while no row exists (expected_version 0).
-- name: InsertProviderPreference :one
INSERT INTO project_provider_preferences (project_id, profile_id, version)
VALUES ($1, $2, 1)
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- Compare-and-swap on the version: no row back means the caller's version was stale.
-- name: UpdateProviderPreference :one
UPDATE project_provider_preferences
SET profile_id = $2, version = version + 1
WHERE project_id = $1 AND version = $3
RETURNING *;
