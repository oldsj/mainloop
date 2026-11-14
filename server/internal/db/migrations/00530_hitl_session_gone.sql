-- +goose Up
-- When kagent no longer has a bound session's conversation while its Agent still exists, the
-- observer stops scanning it (docs/specs/inbox.md#go-hitl-observer-and-response-dispatch). The row
-- stays, so the binding is not adopted again; a binding that names another kagent session replaces it.
ALTER TABLE hitl_sessions ADD COLUMN gone_at timestamptz;

-- +goose Down
ALTER TABLE hitl_sessions DROP COLUMN IF EXISTS gone_at;
