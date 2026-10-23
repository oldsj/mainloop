-- +goose Up
-- A task's delegate-chosen model and reasoning effort: the create request's own choice (#318),
-- validated against the resolved provider profile's models/effort_levels before the task is
-- recorded. NULL means the profile's own default_model/default_effort, exactly as if the field
-- had never existed. tasks carries the create request's values so a later attempt (once handoff
-- is installed) can read them back; task_attempts carries what the attempt actually started with,
-- copied from the task when the attempt is made, and never changes after.
ALTER TABLE tasks ADD COLUMN model text
    CHECK (model IS NULL OR model ~ '^[A-Za-z0-9._:/-]{1,200}$');
ALTER TABLE tasks ADD COLUMN effort text
    CHECK (effort IS NULL OR effort ~ '^[a-z]{1,20}$');

ALTER TABLE task_attempts ADD COLUMN model text
    CHECK (model IS NULL OR model ~ '^[A-Za-z0-9._:/-]{1,200}$');
ALTER TABLE task_attempts ADD COLUMN effort text
    CHECK (effort IS NULL OR effort ~ '^[a-z]{1,20}$');

-- An attempt's model and effort are part of its routing, set once when it is made.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_attempt_routing_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.task_id, OLD.number, OLD.profile_id, OLD.native_provider, OLD.configuration_revision, OLD.agent_namespace, OLD.agent_name, OLD.role, OLD.depth, coalesce(OLD.environment, NEW.environment), OLD.initial_ref, OLD.workspace_request_id, OLD.model, OLD.effort)
        IS DISTINCT FROM ROW(NEW.task_id, NEW.number, NEW.profile_id, NEW.native_provider, NEW.configuration_revision, NEW.agent_namespace, NEW.agent_name, NEW.role, NEW.depth, NEW.environment, NEW.initial_ref, NEW.workspace_request_id, NEW.model, NEW.effort) THEN
        RAISE EXCEPTION 'attempt routing is immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_attempt_routing_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.task_id, OLD.number, OLD.profile_id, OLD.native_provider, OLD.configuration_revision, OLD.agent_namespace, OLD.agent_name, OLD.role, OLD.depth, coalesce(OLD.environment, NEW.environment), OLD.initial_ref, OLD.workspace_request_id)
        IS DISTINCT FROM ROW(NEW.task_id, NEW.number, NEW.profile_id, NEW.native_provider, NEW.configuration_revision, NEW.agent_namespace, NEW.agent_name, NEW.role, NEW.depth, NEW.environment, NEW.initial_ref, NEW.workspace_request_id) THEN
        RAISE EXCEPTION 'attempt routing is immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
ALTER TABLE task_attempts DROP COLUMN IF EXISTS effort;
ALTER TABLE task_attempts DROP COLUMN IF EXISTS model;
ALTER TABLE tasks DROP COLUMN IF EXISTS effort;
ALTER TABLE tasks DROP COLUMN IF EXISTS model;
