-- +goose Up
-- Privileged actions, slice 2 (docs/specs/privileged-actions.md): onepassword.item_to_secret, run
-- by serve-executor under its own database role.
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close', 'onepassword.item_to_secret'));

-- The executor's role, mainloop_executor, is created outside Mainloop (by the database operator,
-- with a password the owner sets). This grants it what serve-executor does and no more: read the
-- actions and their events, write the preflight and execution columns, append events. It cannot
-- record a decision (decided_at, approved_digest, decision_note), change what was asked, or touch
-- any other table. `mainloop migrate up` calls it on every run, so a role created after this
-- migration gets its grants at the next deploy. Returns whether the role exists.
-- +goose StatementBegin
CREATE FUNCTION privileged_executor_grants() RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mainloop_executor') THEN
        RETURN false;
    END IF;
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO mainloop_executor',
        (SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE c.oid = 'privileged_actions'::regclass));
    GRANT SELECT ON privileged_actions, privileged_action_events TO mainloop_executor;
    GRANT UPDATE (
        state, preflight_attempts, frozen_facts, digest, attempts, claimed_until, next_attempt_at,
        result, last_error, finished_at, delivery_state, delivery_next_at, updated_at
    ) ON privileged_actions TO mainloop_executor;
    GRANT INSERT ON privileged_action_events TO mainloop_executor;
    GRANT USAGE ON SEQUENCE privileged_action_events_id_seq TO mainloop_executor;
    RETURN true;
END;
$$;
-- +goose StatementEnd
SELECT privileged_executor_grants();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mainloop_executor') THEN
        REVOKE ALL ON privileged_actions, privileged_action_events FROM mainloop_executor;
        REVOKE ALL ON SEQUENCE privileged_action_events_id_seq FROM mainloop_executor;
        EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM mainloop_executor', current_schema());
    END IF;
END;
$$;
-- +goose StatementEnd
DROP FUNCTION IF EXISTS privileged_executor_grants();
DELETE FROM privileged_actions WHERE type = 'onepassword.item_to_secret';
ALTER TABLE privileged_actions DROP CONSTRAINT privileged_actions_type_check;
ALTER TABLE privileged_actions ADD CONSTRAINT privileged_actions_type_check
    CHECK (type IN ('github.pr_merge', 'github.pr_close'));
