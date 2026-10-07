"""Additive b1 migration; receipts are independent of rebuildable observations."""

HITL_MIGRATION_SQL = """
ALTER TABLE projects ADD COLUMN IF NOT EXISTS merge_policy TEXT NOT NULL DEFAULT 'auto'
    CHECK (merge_policy IN ('auto', 'approval'));
ALTER TABLE projects ADD COLUMN IF NOT EXISTS merge_policy_version BIGINT NOT NULL DEFAULT 1
    CHECK (merge_policy_version > 0);
CREATE TABLE IF NOT EXISTS project_merge_policy_audit (
    id BIGSERIAL PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    owner_id TEXT NOT NULL,
    old_policy TEXT NOT NULL CHECK (old_policy IN ('auto','approval')),
    new_policy TEXT NOT NULL CHECK (new_policy IN ('auto','approval')),
    version BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(project_id, version)
);
CREATE TABLE IF NOT EXISTS native_observed_sessions (
    gateway TEXT NOT NULL,
    runtime_session_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(gateway, runtime_session_id)
);
CREATE TABLE IF NOT EXISTS native_hitl_observer_checkpoints (
    gateway TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    checkpoint JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(gateway, owner_id)
);
CREATE TABLE IF NOT EXISTS native_hitl_associations (
    identity_hash TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS native_hitl_requests (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    outer_key TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(owner_id, outer_key)
);
-- Only verified aliases enter this table. Unresolved payload hints have no leaf key.
CREATE TABLE IF NOT EXISTS native_hitl_aliases (
    request_id TEXT NOT NULL REFERENCES native_hitl_requests(id) ON DELETE CASCADE,
    leaf_key TEXT NOT NULL,
    PRIMARY KEY(request_id, leaf_key)
);
CREATE TABLE IF NOT EXISTS native_hitl_responses (
    owner_id TEXT NOT NULL,
    action_id TEXT NOT NULL,
    body_hash TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    outbound_message_id TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(owner_id, action_id)
);
-- Durable uniqueness for every batch member, independent of projection lifetime.
CREATE TABLE IF NOT EXISTS native_hitl_response_members (
    leaf_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    action_id TEXT NOT NULL,
    call_snapshot JSONB NOT NULL,
    FOREIGN KEY(owner_id, action_id) REFERENCES native_hitl_responses(owner_id, action_id)
);
CREATE INDEX IF NOT EXISTS idx_hitl_merge_receipt_key
    ON native_hitl_response_members(owner_id, (call_snapshot->'merge_key'));
CREATE TABLE IF NOT EXISTS native_hitl_response_transport (
    owner_id TEXT NOT NULL,
    action_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'recorded'
        CHECK(state IN ('recorded','sending','accepted','uncertain','rejected_transport')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(owner_id, action_id),
    FOREIGN KEY(owner_id, action_id) REFERENCES native_hitl_responses(owner_id, action_id)
);
ALTER TABLE queue_items ADD COLUMN IF NOT EXISTS hitl_request_id TEXT
    REFERENCES native_hitl_requests(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_hitl_request
    ON queue_items(hitl_request_id) WHERE hitl_request_id IS NOT NULL;

CREATE OR REPLACE FUNCTION immutable_hitl_receipt() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Owner decision receipts are immutable';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS immutable_hitl_response ON native_hitl_responses;
CREATE TRIGGER immutable_hitl_response BEFORE UPDATE OR DELETE ON native_hitl_responses
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
DROP TRIGGER IF EXISTS immutable_hitl_member ON native_hitl_response_members;
CREATE TRIGGER immutable_hitl_member BEFORE UPDATE OR DELETE ON native_hitl_response_members
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
DROP TRIGGER IF EXISTS immutable_policy_audit ON project_merge_policy_audit;
CREATE TRIGGER immutable_policy_audit BEFORE UPDATE OR DELETE ON project_merge_policy_audit
    FOR EACH ROW EXECUTE FUNCTION immutable_hitl_receipt();
"""
