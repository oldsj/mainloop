-- Credentials: authentication lookups and the revoke/cleanup lifecycle. The row CRUD is in core.sql.

-- name: GetMCPActor :one
-- The published MCP credential behind a token hash with its binding and session. A credential
-- whose binding or session is gone matches nothing, like an unknown token.
SELECT
    c.id AS credential_id,
    c.binding_id,
    b.kind AS agent_kind,
    b.role AS role,
    b.mcp_grant_kind AS mcp_grant_kind,
    b.parent_session_id AS parent_session_id,
    s.user_id AS user_id,
    s.status AS session_status,
    s.archived_at AS session_archived_at,
    s.project_id AS project_id
FROM credentials AS c
JOIN agent_bindings AS b ON b.session_id = c.binding_id
JOIN sessions AS s ON s.id = c.binding_id
WHERE c.token_hash = $1 AND c.kind = 'mcp' AND c.state = 'published';

-- name: GetLiveCredential :one
SELECT * FROM credentials
WHERE binding_id = $1 AND kind = $2 AND state IN ('planned', 'published');

-- name: RevokeBindingCredentials :many
-- Revokes the live credentials of one binding and makes the Secret deletes due now. Only this
-- binding's rows change; other bindings' credentials are untouched.
UPDATE credentials
SET state = 'revoked', revoked_at = now(), updated_at = now(), next_attempt_at = now()
WHERE binding_id = $1 AND state IN ('planned', 'published')
RETURNING *;

-- name: RevokeStalePlannedCredentials :many
-- A planned row whose publish never finished: the Secret create may or may not have landed, so
-- the cleanup loop looks the Secret up by name.
UPDATE credentials
SET state = 'revoked', revoked_at = now(), updated_at = now(), next_attempt_at = now()
WHERE state = 'planned' AND updated_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::float8)
RETURNING *;

-- name: RevokeOrphanedCredentials :many
-- Live credentials whose binding no longer exists (it has no foreign key on purpose).
UPDATE credentials AS c
SET state = 'revoked', revoked_at = now(), updated_at = now(), next_attempt_at = now()
WHERE c.state IN ('planned', 'published')
    AND NOT EXISTS (SELECT 1 FROM agent_bindings AS b WHERE b.session_id = c.binding_id)
    AND c.updated_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::float8)
RETURNING *;

-- name: SetBindingGrant :one
UPDATE agent_bindings SET mcp_grant_kind = $2, updated_at = now() WHERE session_id = $1 RETURNING *;

-- name: LockSessionForCredential :one
-- The session behind a binding, locked FOR SHARE so a state change that ends it (which updates
-- the row) waits for the credential plan to commit, and a plan that comes after it sees the end.
SELECT status, archived_at FROM sessions WHERE id = $1 FOR SHARE;

-- name: RevokeEndedSessionCredentials :many
-- Live credentials of sessions that can no longer act (archived, or completed, failed or
-- cancelled). Whatever ended them revokes in its own transaction; this takes what slipped past
-- (a path that did not, or a credential that was planned just before). The predicate is the
-- authenticator's: superseded, idle and waiting sessions keep their credentials.
UPDATE credentials AS c
SET state = 'revoked', revoked_at = now(), updated_at = now(), next_attempt_at = now()
FROM sessions AS s
WHERE s.id = c.binding_id
    AND c.state IN ('planned', 'published')
    AND (s.archived_at IS NOT NULL OR s.status IN ('completed', 'failed', 'cancelled'))
RETURNING c.*;

-- name: MarkCredentialCreateStarted :one
-- Records, before the Secret create, that it is about to start. Only while the row is planned, so a
-- row revoked first never gets a create: cleanup that sees no start time can call the name free.
UPDATE credentials
SET create_started_at = now()
WHERE id = $1 AND state = 'planned'
RETURNING id;

-- name: LockPublishedCredential :one
-- The caller's MCP credential, locked FOR SHARE and required to be published. A revoke updates the
-- row, so it waits for the write transaction that holds this lock, and a write that starts after
-- the revoke finds no published row.
SELECT id FROM credentials WHERE id = $1 AND state = 'published' FOR SHARE;

-- name: LockAgentBinding :one
-- The binding row, locked FOR SHARE until the credential plan commits. A main-provider switch
-- updates this row (it moves the credential identity on), so it waits for the plan, and a plan
-- that comes after it reads the new identity instead of a stale one.
SELECT * FROM agent_bindings WHERE session_id = $1 FOR SHARE;
