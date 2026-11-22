-- Project onboarding proposals (docs/specs/projects.md#onboarding-proposals).

-- The insert does nothing when the owner already used the request id, or when the repository
-- already has an open proposal; the caller then reads which one it was.
-- name: InsertProjectProposal :one
INSERT INTO project_proposals (
    id, owner_id, request_id, repository, repo_key, spec, facts, state, proposer_role,
    proposer_session_id, proposer_task_id
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_id), sqlc.arg(request_id), sqlc.arg(repository),
    lower(sqlc.arg(repository)::text), sqlc.arg(spec), sqlc.arg(facts), 'proposed',
    sqlc.arg(proposer_role), sqlc.narg(proposer_session_id), sqlc.narg(proposer_task_id)
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetProjectProposalByRequest :one
SELECT * FROM project_proposals WHERE owner_id = $1 AND request_id = $2;

-- name: GetOpenProjectProposalForRepo :one
SELECT * FROM project_proposals
WHERE owner_id = $1 AND repo_key = lower(sqlc.arg(repository)::text) AND state IN ('proposed', 'approved');

-- name: GetProjectProposal :one
SELECT * FROM project_proposals WHERE owner_id = $1 AND id = $2;

-- The owner's project for a repository, matched as imports match it (case-insensitive).
-- name: GetOwnedProjectIDByRepo :one
SELECT id FROM projects WHERE user_id = $1 AND lower(full_name) = lower(sqlc.arg(repository)::text);
