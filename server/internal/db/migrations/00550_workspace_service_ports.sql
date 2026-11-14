-- +goose Up
-- The preview ports of a project's workspaces (docs/specs/workspaces.md#workspace-service): a JSON
-- array of {name, number}, declared with the workspace service. A workspace created without ports
-- of its own copies them into workspaces.ports, so a later change never alters an existing
-- workspace or a replayed create.
ALTER TABLE projects
    ADD COLUMN workspace_service_ports jsonb NOT NULL DEFAULT '[]'
        CHECK (jsonb_typeof(workspace_service_ports) = 'array'
            AND jsonb_array_length(workspace_service_ports) <= 16);

-- +goose Down
ALTER TABLE projects DROP COLUMN workspace_service_ports;
