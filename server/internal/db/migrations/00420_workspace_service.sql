-- +goose Up
-- A project's workspace service (docs/specs/workspaces.md): the argv of a long-running process, such
-- as a development server, that kagent starts in the checkout at every start and resume of a
-- workspace's runtime. Empty declares none. A workspace stores the command it sends kagent when it
-- is created (the project's, run through env(1) with its preview URLs), so a later change never
-- makes a replayed create a different request.
ALTER TABLE projects
    ADD COLUMN workspace_service_command text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(workspace_service_command) <= 61
            AND array_position(workspace_service_command, '') IS NULL);
ALTER TABLE workspaces
    ADD COLUMN service_command text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(service_command) <= 64 AND array_position(service_command, '') IS NULL);

-- +goose Down
ALTER TABLE workspaces DROP COLUMN service_command;
ALTER TABLE projects DROP COLUMN workspace_service_command;
