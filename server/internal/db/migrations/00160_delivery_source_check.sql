-- +goose Up
-- Who wrote a delivery's message: the owner, or Mainloop on the owner's behalf. The contract's
-- Message.source enum is these two values, so the column holds only them.
ALTER TABLE deliveries ADD CONSTRAINT deliveries_source_check CHECK (source IN ('user', 'mainloop'));

-- +goose Down
ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_source_check;
