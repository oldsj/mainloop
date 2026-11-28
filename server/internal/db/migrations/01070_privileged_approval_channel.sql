-- +goose Up
-- Every approval of a privileged action records the channel it came through
-- (docs/specs/privileged-actions.md, "Approval channels"): the web app, which carries no
-- signature, or a phone's Touch ID-signed answer. An action decided before this was recorded gets
-- the channel its record shows: a recorded signed approval is phone-signed, any other approval
-- came from the web app.
ALTER TABLE privileged_actions
    ADD COLUMN approval_channel text CHECK (approval_channel IN ('web', 'phone-signed'));

UPDATE privileged_actions p
SET approval_channel = CASE WHEN EXISTS (SELECT 1 FROM privileged_approvals a WHERE a.action_id = p.id)
        THEN 'phone-signed' ELSE 'web' END
WHERE p.approved_digest IS NOT NULL;

-- Only an approved action has a channel, and every state past an approval has one.
ALTER TABLE privileged_actions
    ADD CONSTRAINT privileged_actions_approval_channel CHECK (
        (approval_channel IS NULL) = (approved_digest IS NULL)
        AND (NOT privileged_action_past_approval(state) OR approval_channel IS NOT NULL));

-- +goose Down
ALTER TABLE privileged_actions
    DROP CONSTRAINT privileged_actions_approval_channel,
    DROP COLUMN approval_channel;
