-- Project memory (docs/specs/project-context.md). Writers hold the project row lock
-- (LockOwnedProject), so sequence numbers and the active byte total are read and advanced by one
-- writer at a time.

-- name: ProjectMemorySeq :one
SELECT coalesce(max(greatest(activated_seq, coalesce(ended_seq, 0))), 0)::bigint
FROM project_memory_entries WHERE project_id = $1;

-- name: ActiveProjectMemoryBytes :one
SELECT coalesce(sum(octet_length(text)), 0)::bigint
FROM project_memory_entries WHERE project_id = $1 AND state = 'active';

-- name: CountProposedProjectMemory :one
SELECT count(*) FROM project_memory_entries WHERE project_id = $1 AND state = 'proposed';

-- name: InsertProjectMemoryEntry :one
INSERT INTO project_memory_entries (
    id, project_id, text, state, author, source_session_id, source_task_id, source_attempt_id,
    source_delivery_id, replaces_id, activated_seq, decided_at
) VALUES (
    sqlc.arg(id), sqlc.arg(project_id), sqlc.arg(text), sqlc.arg(state), sqlc.arg(author),
    sqlc.narg(source_session_id), sqlc.narg(source_task_id), sqlc.narg(source_attempt_id),
    sqlc.narg(source_delivery_id), sqlc.narg(replaces_id), sqlc.narg(activated_seq),
    CASE WHEN sqlc.arg(state)::text = 'active' THEN now() END
) RETURNING *;

-- name: GetProjectMemoryEntry :one
SELECT * FROM project_memory_entries WHERE id = $1 AND project_id = $2;

-- An open entry with this exact text: a repeated remember answers it instead of adding another.
-- name: FindOpenProjectMemoryByText :one
SELECT * FROM project_memory_entries
WHERE project_id = $1 AND text = $2 AND state IN ('active', 'proposed')
ORDER BY created_at, id
LIMIT 1;

-- Active entries first, then proposals, each oldest first.
-- name: ListOpenProjectMemory :many
SELECT * FROM project_memory_entries
WHERE project_id = $1 AND state IN ('active', 'proposed')
ORDER BY state = 'proposed', coalesce(activated_seq, 0), created_at, id;

-- The entries active at a memory sequence, in the order they became active.
-- name: ListProjectMemoryAt :many
SELECT * FROM project_memory_entries
WHERE project_id = sqlc.arg(project_id)
  AND activated_seq IS NOT NULL AND activated_seq <= sqlc.arg(seq)::bigint
  AND (ended_seq IS NULL OR ended_seq > sqlc.arg(seq)::bigint)
ORDER BY activated_seq, id;

-- name: ActivateProjectMemoryEntry :one
UPDATE project_memory_entries SET state = 'active', activated_seq = sqlc.arg(seq)::bigint, decided_at = now()
WHERE id = sqlc.arg(id) AND state = 'proposed'
RETURNING *;

-- Ends an active entry as forgotten or replaced.
-- name: EndProjectMemoryEntry :one
UPDATE project_memory_entries SET state = sqlc.arg(state), ended_seq = sqlc.arg(seq)::bigint, decided_at = now()
WHERE id = sqlc.arg(id) AND state = 'active'
RETURNING *;

-- Closes a proposal as rejected or withdrawn.
-- name: CloseProjectMemoryProposal :one
UPDATE project_memory_entries SET state = sqlc.arg(state), decided_at = now()
WHERE id = sqlc.arg(id) AND state = 'proposed'
RETURNING *;
