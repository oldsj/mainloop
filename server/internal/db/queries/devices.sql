-- name: InsertOwnerDevice :one
-- No row when the key, or the registering token, was registered before.
INSERT INTO owner_devices (id, owner_id, name, platform, request_key_x, request_key_y, key_thumbprint, registered_by,
                           registration_jti, registration_auth_time)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetOwnerDevice :one
SELECT * FROM owner_devices WHERE id = $1;

-- name: ListOwnerDevices :many
SELECT * FROM owner_devices WHERE owner_id = $1 ORDER BY created_at DESC, id LIMIT 100;

-- name: RevokeOwnerDevice :one
-- Revokes once; a second call returns the row unchanged.
UPDATE owner_devices SET revoked_at = coalesce(revoked_at, now())
WHERE id = $1 AND owner_id = $2
RETURNING *;

-- name: TouchOwnerDevice :exec
-- Records a verified request, at most once a minute per device.
UPDATE owner_devices SET last_seen_at = now()
WHERE id = $1 AND revoked_at IS NULL AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 minute');

-- name: UseDeviceToken :execrows
-- Records a device write's token; no row when it was used before.
INSERT INTO device_token_uses (device_id, jti, expires_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: PruneDeviceTokenUses :execrows
-- A used token past its expiry is refused for that reason alone.
DELETE FROM device_token_uses WHERE expires_at < now();
