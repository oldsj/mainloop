-- +goose Up
-- Pull requests and merges. Both are durable intents that a reconcile loop carries out: the row is
-- written first, with an explicit state and an idempotency key, and GitHub is only called from a
-- claimed row with no transaction open. A GitHub write whose outcome is not known is never sent
-- again; the loop reads the remote and settles the row from what it finds (docs/specs).
--
--   pr_creations.state
--     pending      written; nothing has been sent
--     dispatching  the claimer verified the branch and found no PR, and is about to POST (or did)
--     uncertain    the POST may or may not have created a PR; resolved by listing PRs for the head
--     created      final: the PR exists (pr_number, url)
--     refused      final: GitHub or Mainloop refused it, or no PR appeared (detail says why)
--   merge_candidates.state
--     prepared     a proposal exists; nobody asked to merge it
--     evaluating   a merge was requested (auto policy, or the owner's exact consent): the loop reads
--                  the evidence until it holds, fails, or the deadline passes
--     merging      the merge intent is committed; the one PUT is being sent (or was)
--     uncertain    the PUT's outcome is not known; resolved by reading the PR, never by resending
--     blocked      final: the evidence or GitHub refused it (detail says why)
--     expired      final: the deadline passed before the evidence held
--     superseded   final: a newer head or proposal replaced it
--     merged       final: the PR is merged
--
-- The ledger (proposals, request keys, results) is append-only: one trigger, created here with the
-- tables, refuses every update and delete.

CREATE TABLE pr_creations (
    id text PRIMARY KEY,
    -- "pr:<owner>:<request id>": a replayed tool call finds its row by this key.
    idempotency_key text NOT NULL UNIQUE,
    owner_id text NOT NULL,
    project_id text NOT NULL,
    -- The Mainloop session that asked; the PR's task is found through it.
    session_id text,
    request_id text NOT NULL,
    -- sha256 of the canonical arguments without the request id: the same request id with other
    -- arguments is a conflict.
    payload_hash text NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    repo_id bigint NOT NULL,
    repo_full_name text NOT NULL,
    head text NOT NULL,
    base text NOT NULL,
    expected_sha text NOT NULL CHECK (expected_sha ~ '^[0-9a-f]{40}$'),
    title text NOT NULL,
    body text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'dispatching', 'uncertain', 'created', 'refused')
    ),
    pr_number bigint,
    url text,
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    -- Lookups made while uncertain, so the recheck delay can grow.
    rechecks integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'created') = (pr_number IS NOT NULL AND url IS NOT NULL))
);
-- One PR intent in flight per head branch: a second request for the same head finds the first. A
-- finished intent is history, so a branch name can be used for a later PR.
CREATE UNIQUE INDEX pr_creations_head ON pr_creations (owner_id, repo_id, head, base)
WHERE state IN ('pending', 'dispatching', 'uncertain');
CREATE INDEX pr_creations_claim ON pr_creations (next_attempt_at, id)
WHERE state IN ('pending', 'dispatching', 'uncertain');
CREATE INDEX pr_creations_project ON pr_creations (project_id, created_at DESC);

-- One row per preparation: the unit a merge decision applies to, and the merge intent. A new
-- preparation of the same PR supersedes the older ones that have not started merging. The claim
-- columns are reconcile.Table's.
CREATE TABLE merge_candidates (
    id text PRIMARY KEY,
    -- "merge:<owner>:<tool>:<request id>" of the call that prepared it.
    idempotency_key text NOT NULL UNIQUE,
    owner_id text NOT NULL,
    project_id text NOT NULL,
    repository_id bigint NOT NULL,
    repository text NOT NULL,
    pr_number bigint NOT NULL CHECK (pr_number > 0),
    head_sha text NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}$'),
    -- 'auto' merges on the agent's call; 'approval' needs the owner's exact consent.
    route text NOT NULL CHECK (route IN ('auto', 'approval')),
    state text NOT NULL DEFAULT 'prepared' CHECK (
        state IN ('prepared', 'evaluating', 'merging', 'uncertain', 'blocked', 'expired', 'superseded', 'merged')
    ),
    deadline timestamptz,
    -- The consent that authorised the merge (a hitl_responses id) and the tool call it covers.
    receipt_action_id text,
    invocation_request_id text,
    -- What the consent was pinned to when evaluation began: the response, the kagent Session and Agent it
    -- was recorded for, and the trusted template mapping that gave the tool its authority (JSON). The
    -- final decision re-checks all of it. NULL on the automatic route.
    consent_pin jsonb,
    detail text,
    merge_sha text CHECK (merge_sha IS NULL OR merge_sha ~ '^[0-9a-f]{40}$'),
    attempts integer NOT NULL DEFAULT 0,
    rechecks integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'merged') = (merge_sha IS NOT NULL))
);
CREATE INDEX merge_candidates_pr ON merge_candidates (owner_id, repository_id, pr_number);
-- The backstop for "never a second merge": at most one candidate per PR may hold the merge intent.
CREATE UNIQUE INDEX merge_candidates_one_merge ON merge_candidates (owner_id, repository_id, pr_number)
WHERE state IN ('merging', 'uncertain', 'merged');
CREATE INDEX merge_candidates_claim ON merge_candidates (next_attempt_at, id)
WHERE state IN ('evaluating', 'merging', 'uncertain');
CREATE INDEX merge_candidates_prepared ON merge_candidates (created_at, id)
WHERE state = 'prepared' AND route = 'approval';

-- What the owner is shown and decides on, frozen when it is made: the evidence facts, the
-- presentation (the contract's MergeSummary) and the digest of that summary.
CREATE TABLE merge_proposals (
    id text PRIMARY KEY,
    candidate_id text NOT NULL REFERENCES merge_candidates (id),
    owner_id text NOT NULL,
    -- The session that prepared it. Only this session's consent counts.
    session_id text NOT NULL,
    facts jsonb NOT NULL,
    presentation jsonb NOT NULL,
    summary_digest text NOT NULL CHECK (summary_digest ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX merge_proposals_candidate ON merge_proposals (candidate_id);

-- Tool-call idempotency: a request id names one call and its arguments for good.
CREATE TABLE merge_request_keys (
    owner_id text NOT NULL,
    tool text NOT NULL CHECK (tool IN ('prepare', 'merge', 'approved_merge')),
    request_id text NOT NULL,
    payload_hash text NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    proposal_id text NOT NULL REFERENCES merge_proposals (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, tool, request_id)
);
CREATE INDEX merge_request_keys_proposal ON merge_request_keys (proposal_id);

-- The final outcome of a proposal, written once.
CREATE TABLE merge_results (
    proposal_id text PRIMARY KEY REFERENCES merge_proposals (id),
    state text NOT NULL CHECK (state IN ('blocked', 'expired', 'superseded', 'merged')),
    result jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION merge_ledger_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Merge ledger rows are immutable';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER merge_proposals_immutable BEFORE UPDATE OR DELETE ON merge_proposals
FOR EACH ROW EXECUTE FUNCTION merge_ledger_immutable();
CREATE TRIGGER merge_request_keys_immutable BEFORE UPDATE OR DELETE ON merge_request_keys
FOR EACH ROW EXECUTE FUNCTION merge_ledger_immutable();
CREATE TRIGGER merge_results_immutable BEFORE UPDATE OR DELETE ON merge_results
FOR EACH ROW EXECUTE FUNCTION merge_ledger_immutable();

-- +goose Down
DROP TABLE IF EXISTS merge_results;
DROP TABLE IF EXISTS merge_request_keys;
DROP TABLE IF EXISTS merge_proposals;
DROP TABLE IF EXISTS merge_candidates;
DROP TABLE IF EXISTS pr_creations;
DROP FUNCTION IF EXISTS merge_ledger_immutable();
