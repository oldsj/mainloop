-- Projects

-- name: CreateProject :one
INSERT INTO projects (
    id, user_id, owner, name, full_name, description, default_branch, avatar_url, html_url
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1;

-- name: GetProjectByFullName :one
SELECT * FROM projects WHERE user_id = $1 AND lower(full_name) = lower($2);

-- name: ListProjects :many
SELECT * FROM projects WHERE user_id = $1 ORDER BY last_used_at DESC, id;

-- name: TouchProject :exec
UPDATE projects SET last_used_at = now() WHERE id = $1;

-- name: UpdateProjectMetadata :one
UPDATE projects
SET description = $2, default_branch = $3, avatar_url = $4, open_pr_count = $5,
    open_issue_count = $6, metadata_updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetProjectMergePolicy :one
UPDATE projects
SET merge_policy = $2, merge_policy_version = merge_policy_version + 1
WHERE id = $1
RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE id = $1;

-- Sessions

-- name: CreateSession :one
INSERT INTO sessions (
    id, user_id, project_id, title, description, prompt, repo_url, branch_name, base_branch, model,
    color
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: ListSessions :many
SELECT * FROM sessions
WHERE user_id = $1 AND (archived_at IS NULL OR sqlc.arg(include_archived)::boolean)
ORDER BY created_at DESC, id;

-- name: SetSessionStatus :one
UPDATE sessions
SET status = $2,
    started_at = CASE WHEN $2 = 'active' THEN coalesce(started_at, now()) ELSE started_at END,
    completed_at = CASE
        WHEN $2 IN ('completed', 'failed', 'cancelled') THEN coalesce(completed_at, now())
        ELSE completed_at
    END
WHERE id = $1
RETURNING *;

-- name: SetSessionOutcome :one
UPDATE sessions
SET summary = $2, error = $3, commit_sha = $4, result = $5
WHERE id = $1
RETURNING *;

-- name: ArchiveSession :execrows
UPDATE sessions SET archived_at = coalesce(archived_at, now()) WHERE id = $1;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE id = $1;

-- Agent bindings

-- name: CreateAgentBinding :one
INSERT INTO agent_bindings (
    session_id, kind, role, mcp_grant_kind, parent_session_id, topic_id, kagent_request_id, model
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetAgentBinding :one
SELECT * FROM agent_bindings WHERE session_id = $1;

-- name: GetAgentBindingByKagentSession :one
-- A retired main Session counts as bound too: the orphan sweep must not delete a kept history Session.
SELECT * FROM (
    SELECT b.* FROM agent_bindings b WHERE b.kagent_session_id = $1
    UNION ALL
    SELECT b.* FROM agent_bindings b JOIN main_provider_history h ON h.session_id = b.session_id
    WHERE h.kagent_session_id = $1
) bound LIMIT 1;

-- name: ListAgentBindingsByParent :many
SELECT * FROM agent_bindings WHERE parent_session_id = $1 ORDER BY created_at, session_id;

-- name: SetBindingKagentSession :one
UPDATE agent_bindings
SET kagent_session_id = $2, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: SetBindingQueueHeld :exec
UPDATE agent_bindings SET queue_held = $2, updated_at = now() WHERE session_id = $1;

-- name: MarkBindingKagentDeleted :exec
UPDATE agent_bindings
SET kagent_deleted_at = coalesce(kagent_deleted_at, now()), updated_at = now()
WHERE session_id = $1;

-- name: DeleteAgentBinding :execrows
DELETE FROM agent_bindings WHERE session_id = $1;

-- Credentials

-- name: CreateCredential :one
INSERT INTO credentials (id, kind, binding_id, token_hash, secret_name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetCredential :one
SELECT * FROM credentials WHERE id = $1;

-- name: ListCredentialsByBinding :many
SELECT * FROM credentials WHERE binding_id = $1 ORDER BY created_at, id;

-- name: MarkCredentialPublished :one
UPDATE credentials
SET state = 'published', secret_uid = $2, updated_at = now()
WHERE id = $1 AND state = 'planned'
RETURNING *;

-- name: MarkCredentialRevoked :one
UPDATE credentials
SET state = 'revoked', secret_uid = coalesce(secret_uid, $2), revoked_at = now(), updated_at = now()
WHERE id = $1 AND state IN ('planned', 'published')
RETURNING *;

-- Topics

-- name: CreateTopic :one
INSERT INTO topics (id, user_id, name) VALUES ($1, $2, $3) RETURNING *;

-- name: GetTopic :one
SELECT * FROM topics WHERE id = $1;

-- name: GetTopicByName :one
SELECT * FROM topics WHERE user_id = $1 AND name = $2;

-- name: ListTopics :many
SELECT * FROM topics WHERE user_id = $1 ORDER BY updated_at DESC, id;

-- name: UpdateTopic :one
UPDATE topics SET status_line = $2, checkpoint = $3, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteTopic :execrows
DELETE FROM topics WHERE id = $1;

-- name: CreateTopicRecord :one
INSERT INTO topic_records (id, topic_id, kind, text, session_id, evidence_ref)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListTopicRecords :many
SELECT * FROM topic_records WHERE topic_id = $1 ORDER BY created_at, id;

-- name: SetTopicRecordStatus :one
UPDATE topic_records SET status = $2 WHERE id = $1 RETURNING *;
