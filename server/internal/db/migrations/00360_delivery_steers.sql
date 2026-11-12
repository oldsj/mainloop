-- +goose Up
-- A steer is a message the owner sent while a turn was running, to be folded into that turn at its
-- next tool boundary, as Claude Code does, instead of running as a turn of its own afterwards. The
-- message is recorded first (session_messages, with the steer's id); the steer is the intent to
-- deliver it into the running turn (delivery_id). It is kept apart from deliveries because it is not
-- a turn: it holds no place in deliveries_one_open, and it settles with the turn it joined.
--
--   recorded   recorded; nothing has been sent
--   sending    the claimer is calling kagent; the outcome is not known yet
--   uncertain  the send may or may not have reached kagent; settled by reading the turn's task
--              (the steer's id in its history), never sent again
--   delivered  kagent took it into the turn; it ends when the turn ends
--   completed, failed, cancelled  final: the turn it joined ended that way
--   converted  final: it never joined the turn (the turn ended first, or kagent refused it), so it
--              became an ordinary delivery with the same id, once
CREATE TABLE delivery_steers (
    -- The message id; also the A2A messageId, so kagent dedups a retried send on it.
    id text PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- The turn it steers.
    delivery_id text NOT NULL REFERENCES deliveries (id) ON DELETE CASCADE,
    source text NOT NULL DEFAULT 'user' CHECK (source IN ('user', 'mainloop')),
    state text NOT NULL DEFAULT 'recorded' CHECK (
        state IN ('recorded', 'sending', 'uncertain', 'delivered', 'completed', 'failed', 'cancelled', 'converted')
    ),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delivery_steers_claim ON delivery_steers (next_attempt_at, id)
WHERE state IN ('recorded', 'sending', 'uncertain', 'delivered');
CREATE INDEX delivery_steers_delivery ON delivery_steers (delivery_id);

-- +goose Down
DROP TABLE delivery_steers;
