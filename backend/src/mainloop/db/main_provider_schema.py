"""Stable main ownership with retained native-provider history."""

MAIN_PROVIDER_MIGRATION_SQL = """
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS main_owner_id TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS main_agent_ref JSONB;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS credential_identity TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS main_provider_pending BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS main_provider_error TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS main_provider_recoverable BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agent_credential_cleanup ADD COLUMN IF NOT EXISTS credential_identity TEXT;
-- A retired binding keeps its contract/history, but cannot reserve current ownership.
UPDATE native_bindings b SET main_owner_id=NULL FROM sessions s
WHERE s.id=b.session_id AND b.main_owner_id IS NOT NULL
  AND (b.role<>'main' OR b.mcp_grant_kind<>'coordination' OR b.token_hash IS NULL
       OR b.kagent_deleted_at IS NOT NULL OR s.archived_at IS NOT NULL
       OR s.status IN ('completed','failed','cancelled'));
UPDATE native_bindings b SET main_owner_id=s.user_id FROM sessions s
WHERE s.id=b.session_id AND b.role='main' AND s.archived_at IS NULL
  AND b.mcp_grant_kind='coordination' AND b.token_hash IS NOT NULL
  AND b.kagent_deleted_at IS NULL AND s.status NOT IN ('completed','failed','cancelled')
  AND b.main_owner_id IS NULL;
-- Fail closed if a preexisting installation already has competing owners.
CREATE UNIQUE INDEX IF NOT EXISTS native_main_owner ON native_bindings(main_owner_id)
WHERE main_owner_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS main_native_history (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    provider TEXT NOT NULL,
    kagent_session_id TEXT,
    snapshot JSONB NOT NULL,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS main_native_history_session ON main_native_history(session_id);
"""
