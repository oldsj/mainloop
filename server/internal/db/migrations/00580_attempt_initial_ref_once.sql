-- +goose Up
-- A retry or reassign of a code task makes its successor attempt before it knows where the
-- successor's workspace starts: the head of the task branch on origin is read only once the old
-- attempt is fenced, so no push of the old attempt can land after it. The successor is inserted
-- with initial_ref NULL and the ref is recorded once, before its workspace is made. initial_ref
-- is still part of the attempt's routing: set once, never changed.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_attempt_routing_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.task_id, OLD.number, OLD.profile_id, OLD.native_provider, OLD.configuration_revision, OLD.agent_namespace, OLD.agent_name, OLD.role, OLD.depth, coalesce(OLD.environment, NEW.environment), coalesce(OLD.initial_ref, NEW.initial_ref), OLD.workspace_request_id, OLD.model, OLD.effort)
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
    IF ROW(OLD.task_id, OLD.number, OLD.profile_id, OLD.native_provider, OLD.configuration_revision, OLD.agent_namespace, OLD.agent_name, OLD.role, OLD.depth, coalesce(OLD.environment, NEW.environment), OLD.initial_ref, OLD.workspace_request_id, OLD.model, OLD.effort)
        IS DISTINCT FROM ROW(NEW.task_id, NEW.number, NEW.profile_id, NEW.native_provider, NEW.configuration_revision, NEW.agent_namespace, NEW.agent_name, NEW.role, NEW.depth, NEW.environment, NEW.initial_ref, NEW.workspace_request_id, NEW.model, NEW.effort) THEN
        RAISE EXCEPTION 'attempt routing is immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
