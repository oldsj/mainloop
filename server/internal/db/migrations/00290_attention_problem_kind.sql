-- +goose Up
-- A task_reconciliation item (session_lost, stalled, runtime_limit, reconciliation or
-- runtime_stop_unresolved) is Mainloop's own recovery work, not an owner decision: the owner has
-- no concrete choice to make until automatic recovery fails, and that recovery is not built yet
-- (docs/specs/task-handoff.md). It now opens with kind 'problem' instead of 'owner_decision', so it
-- stops counting as a Needs-you decision. See internal/attention/queries/attention.sql.
ALTER TABLE attention_items DROP CONSTRAINT attention_items_kind_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_kind_check
    CHECK (kind IN ('approval', 'question', 'owner_decision', 'problem'));

-- Reclassify rows already open under the old kind; a sweep alone would never touch them, since
-- OpenTaskReconciliationAttention's ON CONFLICT only updated kind going forward from this point on.
UPDATE attention_items SET kind = 'problem', updated_at = now()
WHERE source_kind = 'task_reconciliation' AND kind = 'owner_decision';

-- +goose Down
UPDATE attention_items SET kind = 'owner_decision', updated_at = now()
WHERE source_kind = 'task_reconciliation' AND kind = 'problem';

ALTER TABLE attention_items DROP CONSTRAINT attention_items_kind_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_kind_check
    CHECK (kind IN ('approval', 'question', 'owner_decision'));
