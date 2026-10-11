-- name: GetPushWorkspace :one
SELECT w.*, b.kagent_session_id::text AS native_session_id, b.role, b.mcp_grant_kind,
       e.repository, p.default_branch, p.full_name,
       c.generation, c.attempt_id, c.binding_id AS claim_binding_id
FROM workspaces w
JOIN sessions s ON s.id = w.id AND s.user_id = w.user_id AND s.project_id = w.project_id
JOIN agent_bindings b ON b.session_id = w.id
JOIN git_enrollments e ON e.workspace_id = w.id AND e.state = 'published'
JOIN projects p ON p.id = w.project_id AND p.user_id = w.user_id AND lower(p.full_name) = e.repository
JOIN workspace_writer_claims c ON c.owner_id = w.user_id AND c.repository = e.repository
    AND c.branch = w.branch AND c.held
WHERE w.id = $1 AND s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
    AND NOT EXISTS (SELECT 1 FROM workspace_push_reservations r WHERE r.workspace_id = w.id
        AND (r.state = 'revoked' OR (r.state = 'planned' AND r.expires_at <= clock_timestamp())))
    AND b.kagent_deleted_at IS NULL AND b.kagent_session_id IS NOT NULL
    AND b.mcp_grant_kind = 'workspace' AND b.role IN ('agent', 'supervisor', 'child')
    AND ((b.role = 'agent' AND c.binding_id = w.id AND c.attempt_id IS NULL)
      OR (b.role IN ('supervisor', 'child') AND EXISTS (
        SELECT 1 FROM task_attempts a JOIN tasks t ON t.id = a.task_id
        WHERE a.id = c.attempt_id AND a.binding_id = w.id AND a.workspace_id = w.id AND a.session_id = w.id
          AND a.writer_generation = c.generation AND a.state = 'active' AND t.current_attempt_id = a.id
          AND t.owner_id = w.user_id AND t.project_id = w.project_id
          AND a.role = b.role AND t.status NOT IN ('completed', 'failed', 'cancelled')
          AND (t.parent_task_id IS NULL OR EXISTS (
            SELECT 1 FROM tasks parent JOIN task_attempts pa ON pa.id = parent.current_attempt_id
            JOIN sessions ps ON ps.id = pa.session_id AND ps.user_id = parent.owner_id
            JOIN agent_bindings pb ON pb.session_id = ps.id
            WHERE parent.id = t.parent_task_id AND parent.owner_id = t.owner_id AND pa.state = 'active'
              AND parent.status NOT IN ('completed', 'failed', 'cancelled')
              AND ps.archived_at IS NULL AND ps.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
              AND pb.kagent_session_id IS NOT NULL AND pb.kagent_deleted_at IS NULL
              AND (pa.writer_generation IS NULL OR EXISTS (
                SELECT 1 FROM workspace_writer_claims pc WHERE pc.attempt_id = pa.id AND pc.held
                    AND pc.generation = pa.writer_generation AND pc.owner_id = parent.owner_id
                    AND pc.repository = e.repository AND pc.branch = parent.checkout_branch)))))))
FOR SHARE OF w, s, b, e, p;

-- name: GetPushGrant :one
SELECT * FROM git_push_grants WHERE workspace_id = $1 FOR SHARE;

-- name: GetPushCredential :one
SELECT * FROM credentials WHERE id = $1 FOR SHARE;

-- name: GetPushCapability :one
SELECT g.workspace_id, c.id AS credential_id
FROM credentials c JOIN git_push_grants g ON g.credential_id = c.id AND g.workspace_id = c.binding_id
WHERE c.token_hash = $1 AND c.kind = 'git_push' AND c.state = 'published' AND g.state = 'published';

-- name: GetPushPolicy :one
SELECT * FROM git_branch_policies WHERE project_id = $1 FOR SHARE;

-- name: InsertPushPolicy :exec
INSERT INTO git_branch_policies (project_id, policy) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: UpdatePushPolicy :execrows
UPDATE git_branch_policies SET policy = $3, version = version + 1, updated_at = now()
WHERE project_id = $1 AND version = $2;

-- name: PlanPushGrant :one
INSERT INTO git_push_grants (workspace_id, branch, policy_version, owner_id, writer_generation,
    attempt_id, native_session_id, runtime)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id) DO UPDATE SET updated_at = git_push_grants.updated_at
RETURNING *;

-- name: PublishPushGrant :execrows
UPDATE git_push_grants SET credential_id = $2, state = 'published', updated_at = now()
WHERE workspace_id = $1 AND state = 'planned';

-- name: LockPushWriter :one
SELECT * FROM workspace_writer_claims
WHERE owner_id = $1 AND repository = $2 AND branch = $3 FOR UPDATE;

-- name: PushBranchHeld :one
SELECT EXISTS (SELECT 1 FROM git_publications
WHERE (owner_id = $1 OR owner_id = '') AND repository = $2 AND branch = $3
    AND id <> $4 AND state IN ('pending', 'dispatching', 'unknown'));

-- name: PushAdmissionHeld :one
SELECT EXISTS (SELECT 1 FROM git_push_admissions
WHERE repository = $1 AND branch = $2 AND id <> $3 AND lease_until > clock_timestamp());

-- name: AcquirePushAdmission :execrows
INSERT INTO git_push_admissions (repository, branch, id, lease_until)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repository, branch) DO UPDATE SET id = EXCLUDED.id, lease_until = EXCLUDED.lease_until
WHERE git_push_admissions.lease_until <= clock_timestamp();

-- name: LockPushAdmission :one
SELECT id FROM git_push_admissions WHERE id = $1 AND lease_until > clock_timestamp() FOR UPDATE;

-- name: ReleasePushAdmission :exec
DELETE FROM git_push_admissions WHERE id = $1;

-- name: InsertPushPublication :one
INSERT INTO git_publications (id, workspace_id, request_id, owner_id, repository, branch, attempt,
    transport_evidence, lease_owner, lease_until, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + interval '3 minutes', now() + interval '3 minutes')
RETURNING *;

-- name: GetPushPublication :one
SELECT * FROM git_publications WHERE id = $1;

-- name: GetConfirmedPushBody :one
SELECT * FROM git_publications
WHERE workspace_id = $1 AND attempt->>'credential_id' = sqlc.arg(credential_id)::text
    AND attempt->>'body_digest' = sqlc.arg(body_digest)::text AND state = 'confirmed'
ORDER BY created_at DESC LIMIT 1;

-- name: DispatchPushPublication :execrows
UPDATE git_publications SET state = 'dispatching', updated_at = now(), lease_until = now() + interval '3 minutes',
    next_attempt_at = now() + interval '3 minutes'
WHERE id = $1 AND lease_owner = $2 AND state = 'pending';

-- name: RecordPushValidation :execrows
UPDATE git_publications SET transport_evidence = $3, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND state = 'pending';

-- name: FinishPushPublication :execrows
UPDATE git_publications SET state = $3, transport_receipt = $4, lease_owner = NULL, lease_until = NULL,
    next_attempt_at = now() + interval '1 minute', updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND state IN ('pending', 'dispatching');

-- name: ResolvePushPublication :execrows
UPDATE git_publications SET state = $4, transport_receipt = $5, lease_owner = NULL, lease_until = NULL,
    updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;
