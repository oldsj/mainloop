"""Merge ledger migration; consent remains in the generic immutable HITL tables."""

MERGE_MIGRATION_SQL = """
CREATE TABLE IF NOT EXISTS merge_requests (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    repository_id BIGINT NOT NULL,
    pr_number BIGINT NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('prepared','evaluating','blocked','expired','superseded','merging','uncertain','merged')),
    active_proposal_id TEXT,
    deadline TIMESTAMPTZ,
    intent_id TEXT UNIQUE,
    receipt_action_id TEXT,
    result JSONB,
    UNIQUE(owner_id,repository_id,pr_number,head_sha),
    FOREIGN KEY(owner_id,receipt_action_id) REFERENCES native_hitl_responses(owner_id,action_id)
);
ALTER TABLE merge_requests ADD COLUMN IF NOT EXISTS intent_invocation_id TEXT;
CREATE TABLE IF NOT EXISTS merge_proposals (
    id TEXT PRIMARY KEY,
    candidate_id TEXT NOT NULL REFERENCES merge_requests(id),
    owner_id TEXT NOT NULL,
    binding_id TEXT NOT NULL,
    runtime_session_id TEXT NOT NULL,
    facts JSONB NOT NULL,
    presentation JSONB,
    summary_digest TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE merge_proposals ADD COLUMN IF NOT EXISTS presentation JSONB;
ALTER TABLE merge_proposals ADD COLUMN IF NOT EXISTS summary_digest TEXT;
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname='merge_proposals_summary_digest_format'
            AND conrelid='merge_proposals'::regclass
    ) THEN
        ALTER TABLE merge_proposals ADD CONSTRAINT merge_proposals_summary_digest_format
            CHECK (summary_digest IS NULL OR summary_digest ~ '^[0-9a-f]{64}$');
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS merge_tool_requests (
    owner_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    proposal_id TEXT NOT NULL REFERENCES merge_proposals(id),
    PRIMARY KEY(owner_id,request_id)
);
CREATE TABLE IF NOT EXISTS merge_tool_invocations (
    owner_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    proposal_id TEXT NOT NULL REFERENCES merge_proposals(id),
    PRIMARY KEY(owner_id,request_id)
);
CREATE TABLE IF NOT EXISTS merge_proposal_results (
    proposal_id TEXT PRIMARY KEY REFERENCES merge_proposals(id),
    deadline TIMESTAMPTZ,
    result JSONB NOT NULL
);
DROP TRIGGER IF EXISTS immutable_merge_result ON merge_proposal_results;
CREATE TRIGGER immutable_merge_result BEFORE UPDATE OR DELETE ON merge_proposal_results
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
DROP TRIGGER IF EXISTS immutable_merge_proposal ON merge_proposals;
CREATE TRIGGER immutable_merge_proposal BEFORE UPDATE OR DELETE ON merge_proposals
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
DROP TRIGGER IF EXISTS immutable_merge_request_key ON merge_tool_requests;
CREATE TRIGGER immutable_merge_request_key BEFORE UPDATE OR DELETE ON merge_tool_requests
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
DROP TRIGGER IF EXISTS immutable_merge_invocation ON merge_tool_invocations;
CREATE TRIGGER immutable_merge_invocation BEFORE UPDATE OR DELETE ON merge_tool_invocations
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
"""
