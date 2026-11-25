-- +goose Up
-- Apple push to the owner's registered phones (docs/specs/push-notifications.md). APNs is a second
-- channel behind push_notifications: each notification fans out to one apns_deliveries row per
-- device with a valid token, keyed by notification and device, so sweeps, replicas and restarts
-- never write a second row. The send loop claims a row, rereads whether the owner still needs it
-- and whether the device and its token are still good, and posts with no transaction open.

-- A device's APNs token. One per device; a token belongs to one device (a phone that signed out and
-- registered again moves its token to the new device). created_at is when the device first set a
-- working token: notifications from before it are not sent. A refresh keeps it; setting a token
-- after a refusal resets it.
CREATE TABLE device_push_tokens (
    device_id text PRIMARY KEY REFERENCES owner_devices (id) ON DELETE CASCADE,
    token text NOT NULL UNIQUE CHECK (token ~ '^[0-9a-f]{64,200}$'),
    environment text NOT NULL CHECK (environment IN ('development', 'production')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Set when APNs answers that the token is not (or no longer) valid; pushes stop until the
    -- device sets a token again.
    invalid_at timestamptz,
    invalid_reason text CHECK (char_length(invalid_reason) <= 200)
);

--   pending     written; not sent yet, or an attempt failed and will be retried
--   sent        APNs accepted it
--   suppressed  final: the owner handled it first, or the device was revoked or lost its token
--   failed      final: APNs refused it, or retries ran out
CREATE TABLE apns_deliveries (
    id text PRIMARY KEY,
    -- '<push notification id>:<device id>'.
    idempotency_key text NOT NULL UNIQUE,
    push_id text NOT NULL REFERENCES push_notifications (id) ON DELETE CASCADE,
    device_id text NOT NULL REFERENCES owner_devices (id) ON DELETE CASCADE,
    -- Sent as apns-id on every attempt, so APNs and the logs name one delivery the same way.
    apns_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'suppressed', 'failed')),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz
);
CREATE INDEX apns_deliveries_claim ON apns_deliveries (next_attempt_at, id) WHERE state IN ('pending');
CREATE INDEX apns_deliveries_device ON apns_deliveries (device_id);
CREATE INDEX push_notifications_recent ON push_notifications (owner_id, created_at);

-- With ntfy off, its rows settle 'skipped' instead of waiting for a channel that never comes.
ALTER TABLE push_notifications DROP CONSTRAINT push_notifications_state_check;
ALTER TABLE push_notifications ADD CONSTRAINT push_notifications_state_check
CHECK (state IN ('pending', 'sent', 'suppressed', 'failed', 'skipped'));

-- +goose Down
UPDATE push_notifications SET state = 'suppressed' WHERE state = 'skipped';
ALTER TABLE push_notifications DROP CONSTRAINT push_notifications_state_check;
ALTER TABLE push_notifications ADD CONSTRAINT push_notifications_state_check
CHECK (state IN ('pending', 'sent', 'suppressed', 'failed'));
DROP INDEX push_notifications_recent;
DROP TABLE apns_deliveries;
DROP TABLE device_push_tokens;
