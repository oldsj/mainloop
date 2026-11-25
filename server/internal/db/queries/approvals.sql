-- Touch ID-signed approvals (docs/specs/privileged-actions.md, "Signed approvals").

-- name: RecordPrivilegedApproval :execrows
-- Records a verified approval token in the answer's transaction. No row when the action already
-- has one or the token approved another action.
INSERT INTO privileged_approvals (action_id, device_id, jti, token, signed_at)
VALUES (@action_id, @device_id, @jti, @token, @signed_at)
ON CONFLICT DO NOTHING;

-- name: GetPrivilegedApproval :one
SELECT * FROM privileged_approvals WHERE action_id = @action_id;

-- name: UseExecutorApproval :one
-- The executor's record of the token it acts on: returns the action the token was first used for,
-- and whether this call recorded it.
WITH ins AS (
    INSERT INTO executor_approval_uses (jti, action_id) VALUES (@jti, @action_id)
    ON CONFLICT (jti) DO NOTHING
    RETURNING action_id
)
SELECT action_id, true AS fresh FROM ins
UNION ALL
SELECT action_id, false AS fresh FROM executor_approval_uses
WHERE jti = @jti AND NOT EXISTS (SELECT 1 FROM ins);
