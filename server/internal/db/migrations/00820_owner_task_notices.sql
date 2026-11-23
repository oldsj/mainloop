-- +goose Up
-- The owner's project tasks run under the project's standing supervisor, which is told of each one
-- (docs/specs/project-supervisor.md#owner-tasks). The notice is a third kind of event delivery,
-- sent in the same batch as its children's reports and loop firings.
ALTER TABLE task_event_deliveries
    ADD COLUMN notice_task_id text UNIQUE REFERENCES tasks (id) ON DELETE CASCADE,
    DROP CONSTRAINT task_event_deliveries_one_item,
    ADD CONSTRAINT task_event_deliveries_one_item CHECK (
        num_nonnulls(report_id, loop_firing_id, notice_task_id) = 1
    );

-- +goose Down
DELETE FROM task_event_deliveries WHERE notice_task_id IS NOT NULL;
ALTER TABLE task_event_deliveries
    DROP CONSTRAINT task_event_deliveries_one_item,
    DROP COLUMN notice_task_id,
    ADD CONSTRAINT task_event_deliveries_one_item CHECK (num_nonnulls(report_id, loop_firing_id) = 1);
