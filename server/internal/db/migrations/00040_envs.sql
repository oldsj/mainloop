-- +goose Up
-- Provider preferences and the environment registry. Environment versions are append-only: a
-- trigger rejects UPDATE and DELETE, and every reference to a version is a composite foreign key
-- through its environment, so a project can never select another environment's version.

CREATE TABLE project_provider_preferences (
    project_id text PRIMARY KEY REFERENCES projects (id),
    -- A profile ID or alias from PROVIDER_PROFILES. NULL means "use the installation default".
    profile_id text CHECK (profile_id IS NULL OR length(profile_id) BETWEEN 1 AND 80),
    version bigint NOT NULL CHECK (version > 0)
);

CREATE TABLE dev_environments (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    source_kind text NOT NULL CHECK (source_kind IN ('prebuilt_image', 'definition_repo', 'operator_default')),
    watched_tag text,
    definition jsonb,
    -- The accepted default. Set only through the composite foreign key below.
    default_version_id text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dev_environments_owner ON dev_environments (owner_id, created_at, id);

CREATE TABLE environment_versions (
    id text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES dev_environments (id),
    -- Orders a version list by registration, since ids are random.
    seq bigint GENERATED ALWAYS AS IDENTITY,
    registry text,
    repository text,
    index_digest text CHECK (index_digest IS NULL OR index_digest ~ '^sha256:[0-9a-f]{64}$'),
    platform_manifest_digest text CHECK (
        platform_manifest_digest IS NULL OR platform_manifest_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    config_digest text CHECK (config_digest IS NULL OR config_digest ~ '^sha256:[0-9a-f]{64}$'),
    declared_user text,
    architecture text CHECK (architecture IS NULL OR architecture IN ('amd64', 'arm64')),
    capability_profile jsonb NOT NULL DEFAULT '{}',
    validation_status text NOT NULL CHECK (validation_status IN ('pending_build', 'static_validated')),
    validation_result jsonb NOT NULL DEFAULT '{}',
    validator_version text NOT NULL,
    provenance_kind text NOT NULL CHECK (provenance_kind IN ('user_pushed', 'mainloop_built', 'operator')),
    base_provenance text NOT NULL DEFAULT 'unknown' CHECK (base_provenance IN ('supplied', 'verified', 'unknown')),
    parent_version_id text,
    package_declaration jsonb,
    approval_reference text,
    definition jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id, id),
    FOREIGN KEY (environment_id, parent_version_id) REFERENCES environment_versions (environment_id, id),
    CHECK (package_declaration IS NULL OR parent_version_id IS NOT NULL),
    CHECK (
        validation_status <> 'static_validated' OR (
            registry IS NOT NULL AND repository IS NOT NULL AND platform_manifest_digest IS NOT NULL
            AND config_digest IS NOT NULL AND architecture IS NOT NULL
            AND declared_user = '65532:65532'
        )
    )
);
CREATE INDEX environment_versions_by_environment ON environment_versions (environment_id, seq);

ALTER TABLE dev_environments
ADD CONSTRAINT dev_environments_default_version_fk
FOREIGN KEY (id, default_version_id) REFERENCES environment_versions (environment_id, id);

-- +goose StatementBegin
CREATE FUNCTION reject_environment_version_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Environment versions are immutable';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER immutable_environment_version
BEFORE UPDATE OR DELETE ON environment_versions
FOR EACH ROW EXECUTE FUNCTION reject_environment_version_mutation();

CREATE TABLE environment_grants (
    environment_id text NOT NULL REFERENCES dev_environments (id),
    project_id text NOT NULL REFERENCES projects (id),
    permission text NOT NULL CHECK (permission IN ('use', 'derive')),
    PRIMARY KEY (environment_id, project_id)
);

CREATE TABLE project_environment_selections (
    project_id text PRIMARY KEY REFERENCES projects (id),
    environment_id text NOT NULL REFERENCES dev_environments (id),
    version_id text,
    follow_default boolean NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    FOREIGN KEY (environment_id, version_id) REFERENCES environment_versions (environment_id, id),
    CHECK (follow_default = (version_id IS NULL))
);

-- +goose Down
DROP TABLE IF EXISTS project_environment_selections;
DROP TABLE IF EXISTS environment_grants;
ALTER TABLE IF EXISTS dev_environments DROP CONSTRAINT IF EXISTS dev_environments_default_version_fk;
DROP TABLE IF EXISTS environment_versions;
DROP FUNCTION IF EXISTS reject_environment_version_mutation();
DROP TABLE IF EXISTS dev_environments;
DROP TABLE IF EXISTS project_provider_preferences;
