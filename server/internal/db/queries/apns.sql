-- Apple push (internal/apns): device tokens and one delivery per notification and device.

-- name: ClearPushTokenElsewhere :exec
-- A token belongs to one device: setting it on one takes it from any other.
DELETE FROM device_push_tokens WHERE token = @token AND device_id <> @device_id;

-- name: SetDevicePushToken :one
-- created_at stays while the token works: a refresh does not page for what is already open. After
-- a refusal, setting a token again counts as a new one, so it gets only what is written from now
-- on, not everything that opened while push was stopped.
INSERT INTO device_push_tokens (device_id, token, environment)
VALUES (@device_id, @token, @environment)
ON CONFLICT (device_id) DO UPDATE
SET token = excluded.token, environment = excluded.environment, updated_at = now(),
    created_at = CASE WHEN device_push_tokens.invalid_at IS NULL THEN device_push_tokens.created_at ELSE now() END,
    invalid_at = NULL, invalid_reason = NULL
RETURNING *;

-- name: ClearDevicePushToken :exec
DELETE FROM device_push_tokens WHERE device_id = $1;

-- name: ListDevicePushTokens :many
SELECT t.* FROM device_push_tokens t
JOIN owner_devices d ON d.id = t.device_id
WHERE d.owner_id = $1;

-- name: InvalidateDevicePushToken :execrows
-- Only the token APNs refused: a device that already set a new one keeps it.
UPDATE device_push_tokens SET invalid_at = now(), invalid_reason = @reason
WHERE device_id = @device_id AND token = @token AND invalid_at IS NULL;

-- name: EnqueueAPNsDeliveries :execrows
-- One delivery per recent notification and active device with a valid token, for notifications
-- written after the device first set a token. ntfy's state does not matter: the channels are
-- independent.
INSERT INTO apns_deliveries (id, idempotency_key, push_id, device_id, apns_id)
SELECT 'apns_' || md5(p.id || ':' || d.id), p.id || ':' || d.id, p.id, d.id, gen_random_uuid()
FROM push_notifications p
JOIN owner_devices d ON d.owner_id = p.owner_id AND d.revoked_at IS NULL
JOIN device_push_tokens t ON t.device_id = d.id AND t.invalid_at IS NULL
WHERE p.owner_id = @owner_id
  AND p.created_at >= t.created_at
  AND p.created_at > now() - interval '1 day'
  AND p.state <> 'suppressed'
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetAPNsDelivery :one
SELECT a.id, a.push_id, a.device_id, a.apns_id, a.attempts,
       p.kind AS push_kind, p.source_id, p.title, p.body, p.href, p.priority, p.created_at AS push_created_at,
       d.revoked_at,
       t.token AS token, t.environment AS environment, t.invalid_at AS token_invalid_at
FROM apns_deliveries a
JOIN push_notifications p ON p.id = a.push_id
JOIN owner_devices d ON d.id = a.device_id
LEFT JOIN device_push_tokens t ON t.device_id = a.device_id
WHERE a.id = $1;

-- name: SettleAPNsDelivery :execrows
-- Records the outcome of the current claim (fenced on lease_owner and attempts).
UPDATE apns_deliveries
SET state = @state, detail = @detail, updated_at = now(),
    sent_at = CASE WHEN @state = 'sent' THEN now() ELSE sent_at END,
    lease_owner = NULL, lease_until = NULL
WHERE id = @id AND lease_owner = @lease_owner AND attempts = @attempts;

-- name: SkipNtfyPush :execrows
-- With ntfy off: its pending rows will never be sent.
UPDATE push_notifications SET state = 'skipped', detail = 'ntfy is off', updated_at = now(),
    lease_owner = NULL, lease_until = NULL
WHERE owner_id = $1 AND state = 'pending';

-- name: GetAttentionItem :one
SELECT * FROM attention_items WHERE id = $1 AND owner_id = $2;
