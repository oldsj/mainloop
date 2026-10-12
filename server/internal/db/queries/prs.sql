-- Pull-request creation intents (internal/prs). The "fenced" queries record a claimer's outcome:
-- they match only the row's current claim (id, lease_owner, attempts) and report rows affected.

-- name: InsertPRCreation :execrows
-- The intent, written before anything is sent. Either unique index (the request id, or the head
-- branch) turns a repeat into no row; the caller then reads the row that is there.
INSERT INTO pr_creations (
    id, idempotency_key, owner_id, project_id, session_id, request_id, payload_hash,
    repo_id, repo_full_name, head, base, expected_sha, title, body
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT DO NOTHING;

-- name: GetPRCreationByKey :one
SELECT * FROM pr_creations WHERE idempotency_key = $1;

-- name: GetPRCreationByHead :one
SELECT * FROM pr_creations
WHERE owner_id = $1 AND repo_id = $2 AND head = $3 AND base = $4 AND state IN ('pending', 'dispatching', 'uncertain');

-- name: GetPRCreation :one
SELECT * FROM pr_creations WHERE id = $1;

-- name: MarkPRDispatching :execrows
-- The claimer found no PR for the head and is about to POST. From here a restart only looks.
UPDATE pr_creations
SET state = 'dispatching', detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'pending';

-- name: MarkPRPending :execrows
-- GitHub rate-limited the POST, so it provably did not run: back to pending for another try.
UPDATE pr_creations
SET state = 'pending', detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'dispatching';

-- name: MarkPRUncertain :one
-- updated_at is when the outcome first became unknown, so a recheck keeps it.
UPDATE pr_creations
SET state = 'uncertain', detail = $4,
    rechecks = CASE WHEN state = 'uncertain' THEN rechecks + 1 ELSE 0 END,
    updated_at = CASE WHEN state = 'uncertain' THEN updated_at ELSE now() END
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain')
RETURNING rechecks, updated_at;

-- name: FinishPRCreated :execrows
UPDATE pr_creations
SET state = 'created', pr_number = $4, url = $5, detail = NULL,
    lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain');

-- name: FinishPRRefused :execrows
UPDATE pr_creations
SET state = 'refused', detail = $4, lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain');
