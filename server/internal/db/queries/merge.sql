-- Merge preparation, evaluation and execution (internal/merge). The "fenced" queries record a
-- claimer's outcome: they match only the row's current claim (id, lease_owner, attempts) and
-- report rows affected, so a claimer that lost its lease changes nothing.

-- name: LockMergeScope :exec
-- Serialises preparations of one PR and one request id for the length of a transaction. Nothing
-- in such a transaction waits on the network.
SELECT pg_advisory_xact_lock(hashtextextended(@scope::text, 0));

-- name: GetMergeRequestKey :one
SELECT * FROM merge_request_keys WHERE owner_id = $1 AND tool = $2 AND request_id = $3;

-- name: InsertMergeRequestKey :exec
INSERT INTO merge_request_keys (owner_id, tool, request_id, payload_hash, proposal_id)
VALUES ($1, $2, $3, $4, $5);

-- name: InsertMergeCandidate :exec
INSERT INTO merge_candidates (
    id, idempotency_key, owner_id, project_id, repository_id, repository, pr_number, head_sha, route
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertMergeProposal :exec
INSERT INTO merge_proposals (
    id, candidate_id, owner_id, session_id, facts, presentation, summary_digest
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertMergeResult :exec
INSERT INTO merge_results (proposal_id, state, result) VALUES ($1, $2, $3)
ON CONFLICT (proposal_id) DO NOTHING;



-- name: GetMergeView :one
-- The proposal with the candidate that answers to it.
SELECT p.id AS proposal_id, p.owner_id, p.session_id, p.facts, p.presentation,
       p.summary_digest, p.created_at AS proposal_created_at,
       c.id AS candidate_id, c.project_id, c.repository_id, c.repository, c.pr_number, c.head_sha,
       c.route, c.state, c.deadline, c.receipt_action_id, c.invocation_request_id, c.detail, c.merge_sha,
       c.updated_at
FROM merge_proposals p
JOIN merge_candidates c ON c.id = p.candidate_id
WHERE p.id = $1 AND p.owner_id = $2;

-- name: SupersedeOlderMergeCandidates :many
-- A new preparation replaces the candidates for the same PR that have not started merging.
UPDATE merge_candidates
SET state = 'superseded', detail = 'replaced by a newer preparation', lease_owner = NULL,
    lease_until = NULL, updated_at = now()
WHERE owner_id = $1 AND repository_id = $2 AND pr_number = $3 AND id <> $4
  AND state IN ('prepared', 'evaluating')
RETURNING id;

-- name: GetMergeInFlight :one
-- A candidate for the PR that holds the merge intent: merging, uncertain or already merged.
SELECT id, state, head_sha FROM merge_candidates
WHERE owner_id = $1 AND repository_id = $2 AND pr_number = $3 AND state IN ('merging', 'uncertain', 'merged')
LIMIT 1;


-- name: BeginMergeEvaluation :one
-- prepared -> evaluating, once. A repeat finds the row already evaluating and changes nothing.
UPDATE merge_candidates
SET state = 'evaluating', deadline = $2, receipt_action_id = $3, invocation_request_id = $4, consent_pin = $5,
    next_attempt_at = now(), detail = NULL, updated_at = now()
WHERE id = $1 AND state = 'prepared'
RETURNING id;

-- name: GetMergeWork :one
SELECT c.id AS candidate_id, c.owner_id, c.project_id, c.repository_id, c.repository, c.pr_number,
       c.head_sha, c.route, c.state, c.deadline, c.receipt_action_id, c.consent_pin, c.detail, c.rechecks, c.updated_at,
       p.id AS proposal_id, p.facts, p.summary_digest, p.session_id
FROM merge_candidates c
JOIN merge_proposals p ON p.candidate_id = c.id
WHERE c.id = $1;

-- name: MarkMergeMerging :execrows
-- The merge intent is committed. A unique violation means another candidate holds it already.
UPDATE merge_candidates
SET state = 'merging', detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'evaluating';

-- name: MarkMergeEvaluating :execrows
-- A merge that GitHub rate-limited provably did not happen: back to evaluating for another try.
UPDATE merge_candidates
SET state = 'evaluating', detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'merging';

-- name: MarkMergeUncertain :one
-- updated_at is when the outcome first became unknown, so a recheck keeps it.
UPDATE merge_candidates
SET state = 'uncertain', detail = $4,
    rechecks = CASE WHEN state = 'uncertain' THEN rechecks + 1 ELSE 0 END,
    updated_at = CASE WHEN state = 'uncertain' THEN updated_at ELSE now() END
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('merging', 'uncertain')
RETURNING rechecks, updated_at;

-- name: FinishMergeCandidate :execrows
-- Moves the claimed row to a final state and releases the lease.
UPDATE merge_candidates
SET state = $4, detail = $5, merge_sha = $6, lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('evaluating', 'merging', 'uncertain');


-- name: ListLiveResponsesForSession :many
-- The session's own live decisions on tool approvals, newest first, with the request they answer and
-- the runtime they were recorded for beside the runtime the session is bound to now.
SELECT r.id AS response_id, r.receipt, q.id AS request_id, q.payload, q.kind, b.kind AS agent_kind,
       q.kagent_session_id AS request_kagent_session_id, q.agent_namespace, q.agent_name,
       b.kagent_session_id AS bound_kagent_session_id, b.kagent_deleted_at, s.archived_at, s.user_id AS session_owner
FROM hitl_responses r
JOIN hitl_requests q ON q.id = r.request_id
JOIN agent_bindings b ON b.session_id = q.session_id
JOIN sessions s ON s.id = q.session_id
WHERE r.owner_id = $1 AND q.session_id = $2 AND q.kind = 'tool_approval'
  AND r.state IN ('recorded', 'sending', 'accepted', 'uncertain')
ORDER BY r.created_at DESC, r.id DESC
LIMIT 50;

-- name: ListPreparedApprovalProposals :many
-- Proposals whose candidate waits for the owner's consent: the sweep looks for a live decision.
SELECT p.id AS proposal_id, p.owner_id, p.session_id, c.id AS candidate_id
FROM merge_candidates c
JOIN merge_proposals p ON p.candidate_id = c.id
WHERE c.state = 'prepared' AND c.route = 'approval'
ORDER BY c.created_at, c.id
LIMIT $1;


-- name: GetResponseState :one
SELECT state FROM hitl_responses WHERE id = $1;

-- name: GetProjectAuthority :one
-- The project the tool call names, for the invoking session's owner. The GitHub repository id is
-- not stored on the project; callers take it from GitHub.
SELECT id, user_id, owner, name, full_name, default_branch, merge_policy, merge_policy_version
FROM projects WHERE id = $1 AND user_id = $2;

-- name: LockProjectForMerge :one
-- The final merge decision takes the project row in share mode: a policy write holds it exclusively,
-- so the decision sees either the old policy or the new one, and a write waits for the decision.
SELECT merge_policy, merge_policy_version FROM projects WHERE id = $1 AND user_id = $2 FOR SHARE;

-- name: LockResponseForMerge :one
-- A withdrawal updates the response row; sharing it orders the decision with the withdrawal.
SELECT state FROM hitl_responses WHERE id = $1 FOR SHARE;

-- name: LockBindingForMerge :one
-- The session's live runtime, shared so that replacing or archiving it waits for the decision.
SELECT b.kind, b.kagent_session_id, b.kagent_deleted_at, s.archived_at, s.user_id
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
WHERE b.session_id = $1
FOR SHARE OF b, s;
