"""Additive environment migration. Versions are append-only, including candidates."""

ENVIRONMENT_MIGRATION_SQL = """
CREATE TABLE IF NOT EXISTS dev_environments (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    default_version_id TEXT
);
CREATE TABLE IF NOT EXISTS environment_versions (
    id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL REFERENCES dev_environments(id),
    snapshot JSONB NOT NULL,
    UNIQUE(environment_id, id)
);
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='environment_default_fk'
                   AND conrelid='dev_environments'::regclass) THEN
        ALTER TABLE dev_environments ADD CONSTRAINT environment_default_fk
        FOREIGN KEY(id, default_version_id) REFERENCES environment_versions(environment_id,id);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS environment_grants (
    environment_id TEXT NOT NULL REFERENCES dev_environments(id),
    project_id TEXT NOT NULL REFERENCES projects(id),
    permission TEXT NOT NULL CHECK(permission IN ('use','derive')),
    PRIMARY KEY(environment_id,project_id)
);
CREATE TABLE IF NOT EXISTS project_environment_selections (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    environment_id TEXT NOT NULL REFERENCES dev_environments(id),
    version_id TEXT,
    follow_default BOOLEAN NOT NULL,
    revision BIGINT NOT NULL CHECK(revision > 0),
    FOREIGN KEY(environment_id,version_id) REFERENCES environment_versions(environment_id,id),
    CHECK(follow_default = (version_id IS NULL))
);
CREATE OR REPLACE FUNCTION reject_environment_version_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'Environment versions are immutable';
END $$;
DROP TRIGGER IF EXISTS immutable_environment_version ON environment_versions;
CREATE TRIGGER immutable_environment_version BEFORE UPDATE OR DELETE ON environment_versions
FOR EACH ROW EXECUTE FUNCTION reject_environment_version_mutation();
"""
