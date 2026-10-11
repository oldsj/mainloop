-- name: GetProjectOwner :one
SELECT user_id FROM projects WHERE id = $1;

-- Selection writers lock the project row first. FOR NO KEY UPDATE does not conflict with the
-- KEY SHARE lock a grant insert takes on this unchanged project key.
-- name: LockProjectForSelection :one
SELECT user_id FROM projects WHERE id = $1 FOR NO KEY UPDATE;

-- name: InsertDevEnvironment :one
INSERT INTO dev_environments (id, owner_id, name, source_kind, watched_tag, definition)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDevEnvironment :one
SELECT * FROM dev_environments WHERE id = $1;

-- name: LockDevEnvironment :one
SELECT * FROM dev_environments WHERE id = $1 FOR UPDATE;

-- name: ListDevEnvironments :many
SELECT * FROM dev_environments WHERE owner_id = $1 ORDER BY created_at, id;

-- name: SetDevEnvironmentDefault :one
UPDATE dev_environments SET default_version_id = $2 WHERE id = $1 RETURNING *;

-- name: InsertEnvironmentVersion :one
INSERT INTO environment_versions (
    id, environment_id, registry, repository, index_digest, platform_manifest_digest,
    config_digest, declared_user, architecture, capability_profile, validation_status,
    validation_result, validator_version, provenance_kind, base_provenance, parent_version_id,
    package_declaration, approval_reference, definition
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19
)
RETURNING *;

-- name: GetEnvironmentVersion :one
SELECT * FROM environment_versions WHERE environment_id = $1 AND id = $2;

-- name: ListEnvironmentVersions :many
SELECT * FROM environment_versions WHERE environment_id = $1 ORDER BY seq;

-- name: UpsertEnvironmentGrant :one
INSERT INTO environment_grants (environment_id, project_id, permission)
VALUES ($1, $2, $3)
ON CONFLICT (environment_id, project_id) DO UPDATE SET permission = excluded.permission
RETURNING *;

-- name: DeleteEnvironmentGrant :exec
DELETE FROM environment_grants WHERE environment_id = $1 AND project_id = $2;

-- name: GetEnvironmentGrant :one
SELECT * FROM environment_grants WHERE environment_id = $1 AND project_id = $2;

-- name: GetEnvironmentSelection :one
SELECT * FROM project_environment_selections WHERE project_id = $1;

-- name: UpsertEnvironmentSelection :one
INSERT INTO project_environment_selections (project_id, environment_id, version_id, follow_default, revision)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id) DO UPDATE SET
    environment_id = excluded.environment_id,
    version_id = excluded.version_id,
    follow_default = excluded.follow_default,
    revision = excluded.revision
RETURNING *;
