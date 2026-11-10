-- +goose Up
-- Every turn now names its model and effort explicitly for every provider (see
-- server/internal/sessions turnMetadata), so whether a session ever had an override no longer
-- matters. The flag carried no other information.
ALTER TABLE agent_bindings DROP COLUMN model_effort_touched;

-- +goose Down
-- The history is gone; a session with an override now counts as touched, the best that can be
-- rebuilt from what is kept.
ALTER TABLE agent_bindings ADD COLUMN model_effort_touched boolean NOT NULL DEFAULT false;
UPDATE agent_bindings SET model_effort_touched = true
WHERE model_override IS NOT NULL OR effort_override IS NOT NULL;
