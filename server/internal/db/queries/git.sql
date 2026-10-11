-- Git transport: enrollments (which repository a workspace's read capability is bound to) and
-- the authentication lookup. Push grants, publications and branch policies are the push gate's.

-- name: GetGitWorkspace :one
-- The workspace to enroll: its repository URL and whether its session can still act.
SELECT w.id, w.repo_url, s.status AS session_status, s.archived_at AS session_archived_at
FROM workspaces AS w
JOIN sessions AS s ON s.id = w.id
WHERE w.id = $1;

-- name: GetGitEnrollment :one
SELECT * FROM git_enrollments WHERE workspace_id = $1;

-- name: PlanGitEnrollment :one
-- Records the intent. A replay finds the row and changes nothing; a revoked row starts over as
-- planned, so a workspace whose capability was swept can be enrolled again.
INSERT INTO git_enrollments (workspace_id, repository)
VALUES ($1, $2)
ON CONFLICT (workspace_id) DO UPDATE SET
    state = CASE WHEN git_enrollments.state = 'revoked' THEN 'planned' ELSE git_enrollments.state END,
    repository = CASE WHEN git_enrollments.state = 'revoked' THEN excluded.repository ELSE git_enrollments.repository END,
    credential_id = CASE WHEN git_enrollments.state = 'revoked' THEN NULL ELSE git_enrollments.credential_id END,
    revoked_at = CASE WHEN git_enrollments.state = 'revoked' THEN NULL ELSE git_enrollments.revoked_at END,
    updated_at = CASE WHEN git_enrollments.state = 'revoked' THEN now() ELSE git_enrollments.updated_at END
RETURNING *;

-- name: PublishGitEnrollment :one
-- planned -> published once the credential's Secret exists, and points a published enrollment at
-- a newer credential. No row means the enrollment was revoked meanwhile.
UPDATE git_enrollments
SET state = 'published', credential_id = $2,
    updated_at = CASE WHEN git_enrollments.state = 'published' AND credential_id = $2 THEN updated_at ELSE now() END
WHERE workspace_id = $1 AND state <> 'revoked'
RETURNING *;

-- name: RevokeGitEnrollment :one
UPDATE git_enrollments
SET state = 'revoked', revoked_at = now(), updated_at = now()
WHERE workspace_id = $1 AND state <> 'revoked'
RETURNING *;

-- name: RevokeStrandedGitEnrollments :many
-- Enrollments whose capability is gone: a published one whose read credential is no longer live,
-- and a planned one that has waited out its publish. The authenticator checks the credential
-- itself, so this only keeps the table honest.
UPDATE git_enrollments AS e
SET state = 'revoked', revoked_at = now(), updated_at = now()
WHERE e.state <> 'revoked'
    AND NOT EXISTS (
        SELECT 1 FROM credentials AS c
        WHERE c.binding_id = e.workspace_id AND c.kind = 'git_read' AND c.state IN ('planned', 'published')
    )
    AND e.updated_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::float8)
RETURNING e.workspace_id;

-- name: GetGitReadCapability :one
-- The enrollment behind a read bearer's hash. A credential that is not published, an enrollment
-- that is not, and a session that has ended all match nothing, like an unknown token.
SELECT c.id AS credential_id, e.workspace_id, e.repository
FROM credentials AS c
JOIN git_enrollments AS e ON e.workspace_id = c.binding_id AND e.credential_id = c.id
JOIN sessions AS s ON s.id = c.binding_id
WHERE c.token_hash = $1 AND c.kind = 'git_read' AND c.state = 'published'
    AND e.state = 'published'
    AND s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled');
