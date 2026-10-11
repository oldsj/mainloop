-- Project import, read and merge policy. Shared project queries live in core.sql.

-- name: ImportProject :one
-- One row per repository per owner; a repeat import only touches last_used_at.
INSERT INTO projects (id, user_id, owner, name, full_name, default_branch, html_url)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, lower(full_name)) DO UPDATE SET last_used_at = now()
RETURNING *;

-- name: GetOwnedProject :one
SELECT * FROM projects WHERE id = $1 AND user_id = $2;

-- name: LockOwnedProject :one
SELECT * FROM projects WHERE id = $1 AND user_id = $2 FOR UPDATE;

-- name: ListOwnedProjects :many
SELECT * FROM projects WHERE user_id = $1 ORDER BY last_used_at DESC, id LIMIT $2;

-- name: ListProjectSessions :many
SELECT s.*, ab.kind AS agent_kind, ab.parent_session_id, t.name AS topic
FROM sessions s
LEFT JOIN agent_bindings ab ON ab.session_id = s.id
LEFT JOIN topics t ON t.id = ab.topic_id
WHERE s.user_id = $1 AND s.project_id = $2 AND s.archived_at IS NULL
  AND (ab.role IS NULL OR ab.role <> 'main')
ORDER BY s.created_at DESC, s.id;

-- name: RefreshProjectMetadata :one
-- Applies GitHub's answer without touching the owner's own fields (policy, last use).
UPDATE projects
SET owner = $2, name = $3, full_name = $4, html_url = $5, description = $6, default_branch = $7,
    avatar_url = $8, open_pr_count = $9, open_issue_count = $10, metadata_updated_at = now()
WHERE id = $1 AND user_id = $11
RETURNING *;

-- name: SetOwnedProjectMergePolicy :one
UPDATE projects
SET merge_policy = $2, merge_policy_version = merge_policy_version + 1
WHERE id = $1
RETURNING *;

-- name: InsertMergePolicyAudit :exec
INSERT INTO project_merge_policy_audit (project_id, owner_id, old_policy, new_policy, version)
VALUES ($1, $2, $3, $4, $5);

-- name: ListProjectsNeedingMetadata :many
-- Projects whose GitHub facts are missing (never fetched, or fetched without a default branch)
-- or older than $2, empty ones first and then the longest-unrefreshed.
SELECT * FROM projects
WHERE user_id = $1
  AND (default_branch = '' OR html_url = '' OR metadata_updated_at IS NULL OR metadata_updated_at < $2)
ORDER BY (default_branch = '' OR html_url = '') DESC, metadata_updated_at NULLS FIRST, id
LIMIT $3;
