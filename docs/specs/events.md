# Live updates

`GET /events` is the owner's server-sent event stream. It only tells the client that something
changed; clients re-read state through the normal endpoints, so a missed or duplicated event is
never a correctness problem.

Status: **implemented** in the Go server (`server/internal/events`, `server/internal/reconcile`),
proven by tests against a scratch PostgreSQL. The retired Python backend had its own in-process
bus, now retired. Nothing here is **measured** under load yet. The Go server publishes no
domain events until the slices that own them call `events.Publish`.

## Frames

Each frame has an `event:` name and a one-line JSON `data:` payload from `api/openapi.yaml`.

| Event               | Stored | Meaning                                                     |
| ------------------- | ------ | ----------------------------------------------------------- |
| `connected`         | no     | First frame of every stream; carries `user_id`.             |
| `heartbeat`         | no     | Sent after 30 seconds without another frame.                |
| `session:progress`  | no     | Live state of a running turn: partial reply, status, tools. |
| `inbox:updated`     | yes    | Inbox items changed.                                        |
| `schedule:updated`  | yes    | A schedule changed or started an execution.                 |
| `session:message`   | yes    | A mirrored reply landed in a session conversation.          |
| `task:updated`      | yes    | A task changed; read it with `getTask`.                     |
| `workspace:updated` | yes    | The workspace lifecycle projection changed.                 |

Stored events carry an `id:` line, the outbox row id. `connected` carries the id the client
resumes from: the newest event id on a fresh stream, or the client's own `Last-Event-ID` on a
resume. `heartbeat` has no `id:`, so it never moves the client's resume point. Events reach a
user in id order, and only that user's events are sent.

## Live progress

Status: **implemented** (`server/internal/progress`), proven by Go tests against the fake kagent and by
SSE tests; web behaviour is in [chat](chat.md). **Not measured** against a live kagent.

`session:progress` shows what a turn is doing while it runs. It is **not stored**: it has no `id:`
line and is never replayed, so it never moves the resume point, and the outbox, retention and
`Last-Event-ID` rules above do not apply to it. It is also not a change notification: a client must
not re-read state because of it. Durable state is unchanged. The reply is still mirrored once, when
the turn settles, and announced by `session:message`.

- Each frame is the **whole** live state of one turn (`session_id`, `message_id` of the user message
  the turn answers, `seq`, `started_at`, `status`, `text`, `tools`), not a delta. A client keeps the
  frame with the highest `seq` for its `message_id` and replaces the rest; a lost frame costs nothing.
  `text` keeps the last 16 KiB (`text_truncated`); `tools` keeps the latest 30.
- Frames come from the kagent task stream the delivery loop already reads: text artifacts (folded
  by artifact id, appended when kagent says so), tool call and result artifacts, and task status
  updates. Nothing extra is sent or subscribed to. The state lives in memory, one entry per session
  with a turn in flight, so the delivery loop and the API must run in one process; a restart drops it
  and the page falls back to a busy flag and then the reply.
- Updates are coalesced to at most four frames a second per session: the first goes out at once,
  later ones inside the interval are folded into one trailing frame.
- A stream that connects mid-turn receives the current frame of each running turn right after
  `connected` (and after any replay), so a reload or reconnect shows the partial reply at once.
- A turn is kept in memory until its settle ends it, or a newer message of the same session replaces
  it. It is never dropped for being quiet: kagent sends nothing while a tool runs, so a long build is
  not a reason to forget the turn. `seq` only grows for a message id. The kept state is bounded like
  the frame (the newest 16 KiB of text and 30 tool lines), so a long turn costs a fixed amount.
  A delivery whose session has been deleted ends its turn with a closing frame, so the page does not
  wait for a reply that cannot come. The memory of ended turns (1024 sessions) forgets the session
  whose turn ended longest ago; a session that ends another turn counts as newest.
- The turn ends with a frame with `active: false` that repeats the last text and tool lines, sent
  after the settle commits. `reply_id` names the conversation message that now holds the outcome (the mirrored reply),
  or is absent when the turn stored none. A client keeps the live bubble until it has read that
  message, so the final reply replaces the bubble without a gap or a duplicate.
- `status.kind` is `thinking`, `writing`, `tool` or `waiting`. Tool labels (`Running Bash: go test ./...`,
  `Reading file.go`) are derived by Mainloop from the tool name and arguments. A turn that waits for the
  owner reads `Waiting for you`.

## Resuming

A client that reconnects with `Last-Event-ID: N` receives every stored event of its user after
`N`, then live events with no gap and no duplicate.

Stored events are kept for 24 hours by default (`MAINLOOP_EVENTS_RETENTION_HOURS`). The server
records the newest pruned id. A resume point older than that, or newer than the newest event
(the database was reset), or not a number, is treated like a fresh connection: nothing is
replayed and the new `connected` frame tells the client to re-read its state. The web client
already refetches everything on every reconnect. If retention prunes past a replay that is still
running, the stream ends and the next attempt starts fresh.

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
  does not wait out open connections. New streams also get `503` while the server is starting,
  until the outbox reader has its starting position; clients retry with backoff (native
  EventSource does not retry a 503).

## Background work

The same package family provides the framework other slices use for side effects
(`server/internal/reconcile`): intent rows with an explicit state and idempotency key, claimed
under a lease with `FOR UPDATE SKIP LOCKED`, retried with capped, jittered backoff, woken by
`pg_notify` and swept periodically. No lock or transaction is held while a handler calls out.
The event retention sweep is its first user.
