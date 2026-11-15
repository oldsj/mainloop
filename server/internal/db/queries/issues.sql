-- Issue write intents (internal/issues). The "fenced" queries record a claimer's outcome: they
-- match only the row's current claim (id, lease_owner, attempts) and report rows affected.

-- name: InsertIssueWrite :execrows
-- The intent, written before anything is sent. A repeat of the request id is no row; the caller
-- then reads the row that is there.
INSERT INTO issue_writes (
    id, idempotency_key, owner_id, project_id, session_id, request_id, payload_hash, kind,
    repo_id, repo_full_name, title, labels, body, issue_number
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetIssueWriteByKey :one
SELECT * FROM issue_writes WHERE idempotency_key = $1;

-- name: GetIssueWrite :one
SELECT * FROM issue_writes WHERE id = $1;

-- name: MarkIssueWriteDispatching :execrows
-- The claimer found no write carrying the marker and is about to POST. From here a restart only
-- looks.
UPDATE issue_writes
SET state = 'dispatching', detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'pending';

-- name: MarkIssueWritePending :execrows
-- GitHub rate-limited the POST, so it provably did not run: back to pending for another try.
UPDATE issue_writes
SET state = 'pending', detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'dispatching';

-- name: MarkIssueWriteUncertain :one
-- updated_at is when the outcome first became unknown, so a recheck keeps it.
UPDATE issue_writes
SET state = 'uncertain', detail = $4,
    rechecks = CASE WHEN state = 'uncertain' THEN rechecks + 1 ELSE 0 END,
    updated_at = CASE WHEN state = 'uncertain' THEN updated_at ELSE now() END
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain')
RETURNING rechecks, updated_at;

-- name: FinishIssueWriteCreated :execrows
UPDATE issue_writes
SET state = 'created', issue_number = $4, comment_id = $5, url = $6, detail = NULL,
    lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain');

-- name: FinishIssueWriteRefused :execrows
UPDATE issue_writes
SET state = 'refused', detail = $4, lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('pending', 'dispatching', 'uncertain');
