-- +goose Up
-- The owner's per-session /model and /effort overrides: sticky until changed, durable across
-- reloads and restarts, and applied from the session's next turn (see server/internal/kagent and
-- server/internal/sessions). NULL means the provider's default. model_effort_touched stays true
-- forever once either override is first set to a non-default value: Codex keeps a turn/start
-- override across its later turns, so once a session has touched either one, every turn names both
-- explicitly, even when a later /model or /effort reset one back to its default.
ALTER TABLE agent_bindings ADD COLUMN model_override text
    CHECK (model_override IS NULL OR model_override ~ '^[A-Za-z0-9._:/-]{1,200}$');
ALTER TABLE agent_bindings ADD COLUMN effort_override text
    CHECK (effort_override IS NULL OR effort_override ~ '^[a-z]{1,20}$');
ALTER TABLE agent_bindings ADD COLUMN model_effort_touched boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS model_effort_touched;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS effort_override;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS model_override;
