-- +goose Up
-- The provider, model and effort of a project's standing supervisor
-- (docs/specs/project-supervisor.md#settings). NULL uses the default: the project's default provider
-- (or the installation's), and the owner's worker session default model and effort for it.
ALTER TABLE projects
    ADD COLUMN supervisor_profile_id text
        CHECK (supervisor_profile_id IS NULL OR length(supervisor_profile_id) BETWEEN 1 AND 80),
    ADD COLUMN supervisor_model text
        CHECK (supervisor_model IS NULL OR supervisor_model ~ '^[A-Za-z0-9._:/-]{1,200}$'),
    ADD COLUMN supervisor_effort text
        CHECK (supervisor_effort IS NULL OR supervisor_effort ~ '^[a-z]{1,20}$');

-- One row per change of a project's supervisor settings, by the owner (API) or the main session
-- (MCP project_settings), with the reassign operation a provider change started. The row and the
-- change commit together; rows are never updated.
CREATE TABLE project_supervisor_settings_audit (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    owner_id text NOT NULL,
    changed_by text NOT NULL CHECK (changed_by IN ('owner', 'main')),
    session_id text,
    old_profile_id text,
    new_profile_id text,
    old_model text,
    new_model text,
    old_effort text,
    new_effort text,
    operation_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((changed_by = 'main') = (session_id IS NOT NULL))
);

CREATE INDEX project_supervisor_settings_audit_project ON project_supervisor_settings_audit (project_id, id);

-- +goose StatementBegin
CREATE FUNCTION project_supervisor_settings_audit_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Supervisor settings audit rows are immutable';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER project_supervisor_settings_audit_immutable BEFORE UPDATE ON project_supervisor_settings_audit
FOR EACH ROW EXECUTE FUNCTION project_supervisor_settings_audit_immutable();

-- +goose Down
DROP TABLE IF EXISTS project_supervisor_settings_audit;
DROP FUNCTION IF EXISTS project_supervisor_settings_audit_immutable();
ALTER TABLE projects
    DROP COLUMN IF EXISTS supervisor_profile_id,
    DROP COLUMN IF EXISTS supervisor_model,
    DROP COLUMN IF EXISTS supervisor_effort;
