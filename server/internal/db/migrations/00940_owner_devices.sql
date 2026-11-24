-- +goose Up
-- The owner's registered phones (docs/specs/devices.md). Each holds a P-256 request key in its
-- Secure Enclave; every API request it makes is a token signed by that key. Only the public key is
-- stored. Revoking a device stops its key at once; rows are kept for the audit.
CREATE TABLE owner_devices (
    id text PRIMARY KEY CHECK (id ~ '^dev_[a-z0-9]{26}$'),
    owner_id text NOT NULL,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    platform text NOT NULL CHECK (platform IN ('ios')),
    -- The request key's JWK coordinates, unpadded base64url (32 bytes each).
    request_key_x text NOT NULL CHECK (request_key_x ~ '^[A-Za-z0-9_-]{43}$'),
    request_key_y text NOT NULL CHECK (request_key_y ~ '^[A-Za-z0-9_-]{43}$'),
    -- RFC 7638 SHA-256 thumbprint of the key: one registration per key, ever.
    key_thumbprint text NOT NULL UNIQUE CHECK (key_thumbprint ~ '^[A-Za-z0-9_-]{43}$'),
    -- The Pocket ID subject whose token registered the device.
    registered_by text NOT NULL CHECK (char_length(registered_by) BETWEEN 1 AND 255),
    -- The registering ID token's jti, when it has one: a token registers one device, ever. (Its
    -- nonce must equal key_thumbprint, which is unique too.)
    registration_jti text UNIQUE CHECK (char_length(registration_jti) BETWEEN 1 AND 255),
    -- When the owner signed in to Pocket ID for this registration (the token's auth_time).
    registration_auth_time timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX owner_devices_owner_created ON owner_devices (owner_id, created_at DESC);

-- A revoked device stays revoked, and a key never changes: guard against an UPDATE that would
-- bring a stolen key back.
-- +goose StatementBegin
CREATE FUNCTION owner_devices_guard() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER owner_devices_guard BEFORE UPDATE ON owner_devices
    FOR EACH ROW EXECUTE FUNCTION owner_devices_guard();

-- A device's write is good once, across restarts and replicas: the token's jti is kept until the
-- token would have expired anyway, and a second use is refused. (Reads use a per-process cache.)
CREATE TABLE device_token_uses (
    device_id text NOT NULL REFERENCES owner_devices (id) ON DELETE CASCADE,
    jti text NOT NULL CHECK (char_length(jti) BETWEEN 16 AND 128),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (device_id, jti)
);
CREATE INDEX device_token_uses_expires ON device_token_uses (expires_at);

-- +goose Down
DROP TABLE device_token_uses;
DROP TRIGGER owner_devices_guard ON owner_devices;
DROP FUNCTION owner_devices_guard();
DROP TABLE owner_devices;
