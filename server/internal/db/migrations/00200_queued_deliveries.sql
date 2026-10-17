-- +goose Up
-- A message to a busy session is now recorded durably as queued instead of refused with 409: the
-- one-open-turn index (deliveries_one_open) and the claim index (deliveries_claim) already list
-- their states explicitly and exclude 'queued' without any change, so many messages may wait their
-- turn behind the open one. The sessions package admits the oldest queued message, one turn at a
-- time, as each turn ends (see docs/specs and internal/sessions/service.go).
ALTER TABLE deliveries DROP CONSTRAINT deliveries_state_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_state_check CHECK (
    state IN ('queued', 'recorded', 'sending', 'delivered', 'completed', 'failed', 'cancelled', 'uncertain')
);

-- +goose Down
ALTER TABLE deliveries DROP CONSTRAINT deliveries_state_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_state_check CHECK (
    state IN ('recorded', 'sending', 'delivered', 'completed', 'failed', 'cancelled', 'uncertain')
);
