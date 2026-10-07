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
    revoked_at TIMESTAMPTZ
);
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
