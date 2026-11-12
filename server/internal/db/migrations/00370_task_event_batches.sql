-- +goose Up
-- Reports to one recipient that wait together (behind its open turn, or inside the short batch
-- window) are sent as one message: their deliveries share one ledger message id. Each report keeps
-- its own delivery row, so its identity and state stay in the ledger; only the message is shared.
ALTER TABLE task_event_deliveries DROP CONSTRAINT task_event_deliveries_delivery_id_key;
CREATE INDEX task_event_deliveries_delivery ON task_event_deliveries (delivery_id) WHERE delivery_id IS NOT NULL;

-- +goose Down
DROP INDEX task_event_deliveries_delivery;
ALTER TABLE task_event_deliveries ADD CONSTRAINT task_event_deliveries_delivery_id_key UNIQUE (delivery_id);
