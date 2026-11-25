-- +goose Up
-- Touch ID-signed approvals (docs/specs/devices.md, docs/specs/privileged-actions.md). A phone
-- registers a second P-256 key, its approval key, whose every signature needs Touch ID. Approving a
-- privileged action from the phone carries a token that key signed for that action's id and
-- digest. serve-api verifies it and records it here; serve-executor verifies it again against keys
-- pinned in its own configuration, never against this database.

-- The approval key, registered with the device or never: all three columns or none.
ALTER TABLE owner_devices
    ADD COLUMN approval_key_x text CHECK (approval_key_x ~ '^[A-Za-z0-9_-]{43}$'),
    ADD COLUMN approval_key_y text CHECK (approval_key_y ~ '^[A-Za-z0-9_-]{43}$'),
    ADD COLUMN approval_key_thumbprint text UNIQUE CHECK (approval_key_thumbprint ~ '^[A-Za-z0-9_-]{43}$'),
    ADD CONSTRAINT owner_devices_approval_key_whole CHECK (
        (approval_key_x IS NULL) = (approval_key_y IS NULL)
        AND (approval_key_x IS NULL) = (approval_key_thumbprint IS NULL)),
    ADD CONSTRAINT owner_devices_approval_key_distinct CHECK (approval_key_thumbprint <> key_thumbprint);

-- The approval key is as immutable as the request key: it can't be added, changed or removed
-- after registration.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION owner_devices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'owner_devices: a revoked device stays revoked';
    END IF;
    IF NEW.id <> OLD.id OR NEW.owner_id <> OLD.owner_id OR NEW.request_key_x <> OLD.request_key_x
        OR NEW.request_key_y <> OLD.request_key_y OR NEW.key_thumbprint <> OLD.key_thumbprint
        OR NEW.registered_by <> OLD.registered_by OR NEW.created_at <> OLD.created_at
        OR NEW.registration_jti IS DISTINCT FROM OLD.registration_jti
        OR NEW.registration_auth_time <> OLD.registration_auth_time
        OR NEW.approval_key_x IS DISTINCT FROM OLD.approval_key_x
        OR NEW.approval_key_y IS DISTINCT FROM OLD.approval_key_y
        OR NEW.approval_key_thumbprint IS DISTINCT FROM OLD.approval_key_thumbprint THEN
        RAISE EXCEPTION 'owner_devices: identity and key columns are immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- A verified approval token, written in the transaction of the answer that carried it. One per
-- action, and a token (by its jti) approves one action, ever.
CREATE TABLE privileged_approvals (
    action_id text PRIMARY KEY REFERENCES privileged_actions (id) ON DELETE CASCADE,
    device_id text NOT NULL REFERENCES owner_devices (id),
    jti text NOT NULL UNIQUE CHECK (char_length(jti) BETWEEN 16 AND 128),
    token text NOT NULL CHECK (char_length(token) BETWEEN 1 AND 4096),
    signed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The executor's own record of the approval tokens it acted on, written before an action's first
-- attempt: a token runs one action, and a retry of that action finds its own record.
CREATE TABLE executor_approval_uses (
    jti text PRIMARY KEY CHECK (char_length(jti) BETWEEN 16 AND 128),
    action_id text NOT NULL,
    used_at timestamptz NOT NULL DEFAULT now()
);

-- The executor also reads the recorded approvals and writes its own uses.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION privileged_executor_grants() RETURNS boolean LANGUAGE plpgsql AS $$
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
    GRANT SELECT ON privileged_approvals TO mainloop_executor;
    GRANT SELECT, INSERT ON executor_approval_uses TO mainloop_executor;
    RETURN true;
END;
$$;
-- +goose StatementEnd
SELECT privileged_executor_grants();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION privileged_executor_grants() RETURNS boolean LANGUAGE plpgsql AS $$
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
DROP TABLE executor_approval_uses;
DROP TABLE privileged_approvals;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION owner_devices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'owner_devices: a revoked device stays revoked';
    END IF;
    IF NEW.id <> OLD.id OR NEW.owner_id <> OLD.owner_id OR NEW.request_key_x <> OLD.request_key_x
        OR NEW.request_key_y <> OLD.request_key_y OR NEW.key_thumbprint <> OLD.key_thumbprint
        OR NEW.registered_by <> OLD.registered_by OR NEW.created_at <> OLD.created_at
        OR NEW.registration_jti IS DISTINCT FROM OLD.registration_jti
        OR NEW.registration_auth_time <> OLD.registration_auth_time THEN
        RAISE EXCEPTION 'owner_devices: identity and key columns are immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER TABLE owner_devices
    DROP COLUMN approval_key_thumbprint, DROP COLUMN approval_key_y, DROP COLUMN approval_key_x;
