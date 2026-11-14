-- +goose Up
-- A 'review' item is a low-priority owner decision: something an agent proposed that waits on the
-- owner, with no turn or task blocked on it. The first is a project's pending memory proposals
-- (source_kind 'project_memory', one item per project; docs/specs/project-context.md). It counts
-- in Needs you, sorts after the other items and sends no push notification.
ALTER TABLE attention_items DROP CONSTRAINT attention_items_kind_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_kind_check
    CHECK (kind IN ('approval', 'question', 'owner_decision', 'problem', 'review'));

-- +goose Down
DELETE FROM attention_items WHERE kind = 'review';

ALTER TABLE attention_items DROP CONSTRAINT attention_items_kind_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_kind_check
    CHECK (kind IN ('approval', 'question', 'owner_decision', 'problem'));
