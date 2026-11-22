-- +goose Up
-- Project onboarding proposals (docs/specs/projects.md#onboarding-proposals): main or a project
-- supervisor proposes importing a repository as a project, and the owner approves or rejects it
-- from one Needs-you card. The row is the durable intent: what was proposed (spec), what Mainloop
-- found when it was proposed (facts), and the owner's decision. Applying an approved proposal
-- through the project APIs is a later slice; it records its steps against this row.
CREATE TABLE project_proposals (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- The proposer's idempotency key: the same request_id returns the same proposal.
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 100),
    -- owner/name as GitHub spells it in the request; repo_key is its lower-case form, which is how
    -- one repository is recognised whatever its case.
    repository text NOT NULL CHECK (char_length(repository) BETWEEN 3 AND 200),
    repo_key text NOT NULL CHECK (repo_key = lower(repository)),
    spec jsonb NOT NULL,
    facts jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('proposed', 'approved', 'rejected', 'applied', 'failed')),
    -- Who proposed it, resolved by the server from the calling session. Plain ids, so the record
    -- outlives a pruned session or task.
    proposer_role text NOT NULL,
    proposer_session_id text,
    proposer_task_id text,
    -- The project the proposal made or found, once applied.
    project_id text REFERENCES projects (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    UNIQUE (owner_id, request_id),
    CHECK ((state = 'proposed') = (decided_at IS NULL))
);
-- At most one undecided or approved-but-unapplied proposal per repository: a second proposal for
-- the same repository returns the open one.
CREATE UNIQUE INDEX project_proposals_one_open ON project_proposals (owner_id, repo_key)
WHERE state IN ('proposed', 'approved');

-- +goose Down
DROP TABLE IF EXISTS project_proposals;
