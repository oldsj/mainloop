-- +goose Up
-- Session defaults gain a third role, 'supervisor': tasks without a parent, project standing
-- supervisors included (docs/specs/chat.md#session-defaults). A supervisor with no supervisor
-- default for a field uses the worker default for it, so existing rows keep their meaning.
ALTER TABLE session_defaults DROP CONSTRAINT session_defaults_role_check;
ALTER TABLE session_defaults ADD CONSTRAINT session_defaults_role_check
    CHECK (role IN ('main', 'supervisor', 'worker'));

-- +goose Down
DELETE FROM session_defaults WHERE role = 'supervisor';
ALTER TABLE session_defaults DROP CONSTRAINT session_defaults_role_check;
ALTER TABLE session_defaults ADD CONSTRAINT session_defaults_role_check
    CHECK (role IN ('main', 'worker'));
