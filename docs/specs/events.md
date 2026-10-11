# Live updates

`GET /events` is the owner's server-sent event stream. It only tells the client that something
changed; clients re-read state through the normal endpoints, so a missed or duplicated event is
never a correctness problem.

Status: **implemented** in the Go server (`server/internal/events`, `server/internal/reconcile`),
proven by tests against a scratch PostgreSQL. The frozen Python backend keeps its own in-process
bus until cutover. Nothing here is **measured** under load yet. The Go server publishes no
domain events until the slices that own them call `events.Publish`.

## Frames

Each frame has an `event:` name and a one-line JSON `data:` payload from `api/openapi.yaml`.

| Event               | Stored | Meaning                                            |
| ------------------- | ------ | -------------------------------------------------- |
| `connected`         | no     | First frame of every stream; carries `user_id`.    |
| `heartbeat`         | no     | Sent after 30 seconds without another frame.       |
| `inbox:updated`     | yes    | Inbox items changed.                               |
| `schedule:updated`  | yes    | A schedule changed or started an execution.        |
| `session:message`   | yes    | A mirrored reply landed in a session conversation. |
| `task:updated`      | yes    | A task changed; read it with `getTask`.            |
| `workspace:updated` | yes    | The workspace lifecycle projection changed.        |

Stored events carry an `id:` line, the outbox row id. `connected` carries the id the client
resumes from: the newest event id on a fresh stream, or the client's own `Last-Event-ID` on a
resume. `heartbeat` has no `id:`, so it never moves the client's resume point. Events reach a
user in id order, and only that user's events are sent.

## Resuming

A client that reconnects with `Last-Event-ID: N` receives every stored event of its user after
`N`, then live events with no gap and no duplicate.

Stored events are kept for 24 hours by default (`MAINLOOP_EVENTS_RETENTION_HOURS`). The server
records the newest pruned id. A resume point older than that, or newer than the newest event
(the database was reset), or not a number, is treated like a fresh connection: nothing is
replayed and the new `connected` frame tells the client to re-read its state. The web client
already refetches everything on every reconnect.

## Delivery and back-pressure

- A change is published in the same database transaction as the domain write, so the event exists
  exactly when the change commits, from any process. A rolled-back change leaves no event.
- Publishing also sends `pg_notify`. The API process listens, wakes, and reads new outbox rows.
  If the listener's connection drops it reconnects, and the reconnect re-reads the outbox, so
  events that arrived meanwhile are delivered late rather than lost. A poll
  (`MAINLOOP_EVENTS_POLL_SECONDS`, default 5) backs this up.
- Each stream has a bounded buffer (`MAINLOOP_EVENTS_SUBSCRIBER_BUFFER`, default 64). Publishers
  never wait for a stream. A client that falls a full buffer behind is dropped: its stream ends,
  and the browser reconnects with `Last-Event-ID` and is replayed from the outbox. A client that
  stops reading entirely is cut off by a 15-second write deadline.
- On server shutdown every stream ends at once and new streams get `503`, so graceful shutdown
  does not wait out open connections.

## Background work

The same package family provides the framework other slices use for side effects
(`server/internal/reconcile`): intent rows with an explicit state and idempotency key, claimed
under a lease with `FOR UPDATE SKIP LOCKED`, retried with capped, jittered backoff, woken by
`pg_notify` and swept periodically. No lock or transaction is held while a handler calls out.
The event retention sweep is its first user.
