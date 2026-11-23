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

-- Records the owner's answer on each proposal whose card was answered: Approve approves it, any
-- other answer rejects it. Approved proposals are due at once.
-- name: DecideAnsweredProjectProposals :many
UPDATE project_proposals p
SET state = CASE WHEN a.detail->>'response' = 'Approve' THEN 'approved' ELSE 'rejected' END,
    decided_at = now(),
    finished_at = CASE WHEN a.detail->>'response' = 'Approve' THEN NULL ELSE now() END,
    next_attempt_at = CASE WHEN a.detail->>'response' = 'Approve' THEN now() END
FROM attention_items a
WHERE p.owner_id = sqlc.arg(owner_id) AND p.state = 'proposed'
  AND a.owner_id = p.owner_id AND a.source_kind = 'queue' AND a.source_id = 'project_proposal_' || p.id
  AND a.detail ? 'response'
RETURNING p.id, p.state, (a.detail->>'response')::text AS response;

-- Claims the oldest due approved proposal for one apply attempt, for lease_seconds.
-- name: ClaimDueProjectProposal :one
UPDATE project_proposals
SET claimed_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision),
    attempts = attempts + 1
WHERE id = (
    SELECT d.id FROM project_proposals d
    WHERE d.owner_id = sqlc.arg(owner_id) AND d.state = 'approved'
      AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= now())
      AND (d.claimed_until IS NULL OR d.claimed_until < now())
    ORDER BY d.created_at, d.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishProjectProposal :execrows
UPDATE project_proposals
SET state = sqlc.arg(state), project_id = sqlc.narg(project_id), last_error = sqlc.narg(last_error),
    finished_at = now(), claimed_until = NULL, next_attempt_at = NULL
WHERE id = sqlc.arg(id) AND state = 'approved';

-- name: RetryProjectProposal :execrows
UPDATE project_proposals
SET last_error = sqlc.arg(last_error), claimed_until = NULL,
    next_attempt_at = now() + make_interval(secs => sqlc.arg(after_seconds)::double precision)
WHERE id = sqlc.arg(id) AND state = 'approved';

-- name: InsertProjectProposalEvent :exec
INSERT INTO project_proposal_events (proposal_id, step, detail)
VALUES (sqlc.arg(proposal_id), sqlc.arg(step), left(sqlc.arg(detail)::text, 2048));

-- name: ListProjectProposalEvents :many
SELECT * FROM project_proposal_events WHERE proposal_id = $1 ORDER BY id;

-- The owner's statically validated environment version of an image: same registry, repository
-- and architecture, and the digest is the version's index or platform manifest digest.
-- name: FindOwnedEnvironmentVersionByImage :one
SELECT v.environment_id, v.id AS version_id
FROM environment_versions v
JOIN dev_environments e ON e.id = v.environment_id
WHERE e.owner_id = sqlc.arg(owner_id) AND v.validation_status = 'static_validated'
  AND v.registry = sqlc.arg(registry) AND v.repository = sqlc.arg(repository)
  AND v.architecture = sqlc.arg(architecture)
  AND (v.index_digest = sqlc.arg(digest) OR v.platform_manifest_digest = sqlc.arg(digest))
ORDER BY v.created_at, v.id
LIMIT 1;
