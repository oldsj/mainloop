"""Standalone push authority and audit schema. Lifecycle integration is deferred."""

PUSH_GATE_MIGRATION_SQL = """
CREATE TABLE IF NOT EXISTS push_branch_policies (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    version BIGINT NOT NULL CHECK(version > 0),
    policy JSONB NOT NULL
);
CREATE TABLE IF NOT EXISTS push_grants (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    owner_id TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    session_id TEXT NOT NULL UNIQUE,
    grant_data JSONB NOT NULL,
    revoked_at TIMESTAMPTZ,
    attempt_id TEXT,
    writer_generation BIGINT CHECK(writer_generation > 0)
);
ALTER TABLE push_grants ADD COLUMN IF NOT EXISTS attempt_id TEXT;
ALTER TABLE push_grants ADD COLUMN IF NOT EXISTS writer_generation BIGINT CHECK(writer_generation > 0);
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='push_grants_writer_proof_pair'
               AND conrelid='push_grants'::regclass) THEN
 ALTER TABLE push_grants ADD CONSTRAINT push_grants_writer_proof_pair
 CHECK((attempt_id IS NULL) = (writer_generation IS NULL));
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS push_publications (
    grant_id TEXT NOT NULL REFERENCES push_grants(id),
    request_id TEXT NOT NULL,
    attempt JSONB NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending','dispatching','confirmed','rejected','unknown')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(grant_id,request_id)
);
"""
