-- +goose Up
-- Push notifications to the owner's phone through ntfy (docs/specs/push-notifications.md). One row
-- per notification is the intent: the enqueue sweep writes it with an idempotency key derived from
-- what it is about, so a repeated sweep, a second replica or a restart never writes a second row
-- for the same item. The send loop claims it, rereads whether the owner still needs to hear it,
-- and posts it with no transaction open.
--
--   pending     written; not sent yet (or a send failed and will be retried after checking ntfy)
--   sent        ntfy accepted it, or a retry found it already on the topic
--   suppressed  final: the owner handled the item (or the task moved on) before it went out
--   failed      final: ntfy refused it, or retries ran out
CREATE TABLE push_notifications (
    id text PRIMARY KEY,
    -- 'attention:<item id>' or 'task:<task id>:<status>:<n>'.
    idempotency_key text NOT NULL UNIQUE,
    owner_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('attention', 'task')),
    -- The attention item or task it is about.
    source_id text NOT NULL,
    -- For a task, the status it reports; for an attention item, the item's kind.
    source_state text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    -- The web app path the notification opens.
    href text NOT NULL,
    priority integer NOT NULL CHECK (priority BETWEEN 1 AND 5),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'suppressed', 'failed')),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz
);
CREATE INDEX push_notifications_claim ON push_notifications (next_attempt_at, id)
WHERE state IN ('pending');
CREATE INDEX push_notifications_source ON push_notifications (kind, source_id, created_at DESC);

-- When notifications were first turned on. Only items and task changes from then on are sent, so
-- enabling push does not page the owner for everything already open.
CREATE TABLE push_floor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    started_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE push_floor;
DROP TABLE push_notifications;
