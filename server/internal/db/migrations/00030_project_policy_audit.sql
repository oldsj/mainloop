-- +goose Up
-- One row per owner change of a project's merge policy. The row and the policy change commit
-- together; rows are never updated or deleted.
CREATE TABLE project_merge_policy_audit (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id),
    owner_id text NOT NULL,
    old_policy text NOT NULL CHECK (old_policy IN ('auto', 'approval')),
    new_policy text NOT NULL CHECK (new_policy IN ('auto', 'approval')),
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, version)
);

-- +goose StatementBegin
CREATE FUNCTION project_policy_audit_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Merge policy audit rows are immutable';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER project_policy_audit_immutable BEFORE UPDATE OR DELETE ON project_merge_policy_audit
FOR EACH ROW EXECUTE FUNCTION project_policy_audit_immutable();

-- +goose Down
DROP TABLE IF EXISTS project_merge_policy_audit;
DROP FUNCTION IF EXISTS project_policy_audit_immutable();
