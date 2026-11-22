-- +goose Up
-- Applying approved project proposals (docs/specs/projects.md#onboarding-proposals). The apply
-- sweep claims an approved proposal with a short lease, applies it step by step through the
-- project APIs with no transaction open, and records every step in project_proposal_events, an
-- append-only audit of the proposal: the owner's decision, each step's result, and the end.
ALTER TABLE project_proposals
    ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at timestamptz,
    ADD COLUMN claimed_until timestamptz,
    ADD COLUMN last_error text,
    ADD COLUMN finished_at timestamptz,
    ADD CONSTRAINT project_proposals_finished CHECK (
        (state IN ('rejected', 'applied', 'failed')) = (finished_at IS NOT NULL)
    );

CREATE INDEX project_proposals_due ON project_proposals (next_attempt_at, created_at)
WHERE state = 'approved';

CREATE TABLE project_proposal_events (
    id bigserial PRIMARY KEY,
    proposal_id text NOT NULL REFERENCES project_proposals (id) ON DELETE CASCADE,
    -- decided, import, environment, verification, workspace_service, applied, failed, retry
    step text NOT NULL CHECK (char_length(step) BETWEEN 1 AND 40),
    detail text NOT NULL CHECK (octet_length(detail) <= 2048),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX project_proposal_events_proposal ON project_proposal_events (proposal_id, id);

-- +goose Down
DROP TABLE IF EXISTS project_proposal_events;
DROP INDEX IF EXISTS project_proposals_due;
ALTER TABLE project_proposals
    DROP CONSTRAINT IF EXISTS project_proposals_finished,
    DROP COLUMN IF EXISTS finished_at,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS claimed_until,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS attempts;
