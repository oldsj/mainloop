# Sessions

Sessions are native Claude Code or Codex work started from the home thread or the `/agents` page. Mainloop keeps the product session, workspace lifecycle, native binding, and message delivery as separate records.

## Go server status (G6a, G6b)

Status: **implemented** for the main session (G6a) and the session routes (G6b) in `server/internal/sessions`; tests use a scratch PostgreSQL and the fake kagent. **Not measured** against a live kagent. The rest of this page describes the frozen Python backend until each slice lands.

- The main session is created by the reconciler (a periodic sweep) and by the first `POST /chat`, never by a GET. Concurrent first calls share one session.
- Delivery states in the Go ledger: `recorded`, `sending`, `delivered`, `completed`, `failed`, `cancelled`, `uncertain`. `queued` arrives with the report queue. Only `recorded` is ever sent: the claimer commits `sending` before the kagent call, holds no database lock or transaction across it, and fences every outcome write on its lease and attempt count, so a claimer that lost its lease changes nothing.
- kagent is retried only when it says the message was not accepted, with the same message id and inside the send retry budget (the kagent client does this). Every other doubt makes the delivery `uncertain`, and it is settled by looking the message up (`GetTask`, else `ListTasks`), never by sending again. A `sending` row reclaimed after a crash goes straight to `uncertain` and is looked up. If no task shows the message once the hold has passed, the delivery fails with "not sent again" and the owner may send a new message.
- Before a send, the loop makes the binding's kagent Session usable: it creates it on first use (idempotent by request id), resumes a suspended one, and waits for it to be ready (`SESSION_READY_WAIT_SECONDS`, 60 by default). When kagent no longer has the Session (its idle TTL, or deleted out of band) the binding gets a new create request id in a write fenced on the delivery's claim, and a new Session is created (the binding's standing hash is cleared for the later standing-context slice). A task parked for input or auth is read again at growing intervals up to `SESSION_PARKED_MAX_SECONDS` (300); a failed read backs off the same way. A delivery whose binding is gone fails. The binding records the create request id and the Agent before every create call (`agent_bindings.kagent_create_request_id`, `kagent_create_agent_namespace`/`_name`). They stay set once the Session id is stored and are cleared only when a create is settled or the Session is replaced, so every reader (the orphan sweep, the resolve sweep) also requires `kagent_session_id IS NULL`. A `resolve-session-creates` sweep (every 5 minutes) settles such a binding whose session has no open delivery: it first rotates the binding's `kagent_request_id` (one UPDATE, fenced on the binding still being unbound, the same create request id and no open delivery), so a message that arrives mid-sweep creates its own Session under the new id and the sweep only deletes the one its replay returns. It then replays the create under the old id with the Agent stored on the binding and the same credential reference the create sent. kagent answering that the id belongs to a deleted Session settles it; `AlreadyExists` (a contract mismatch) also settles it, with a warning, leaving the Session to the orphan sweep; a Session that comes back is deleted (confirmed `DELETED`) first. A later send then finds the id deleted and replaces it as above. A delivery stuck because its Agent cannot be resolved logs at error level once past the uncertain hold; one waiting for a workspace that never gets a Session fails after that hold. A workspace's Session is only ever resumed through the workspace service (`Wake`), never directly. A native or main session follows the configured Agent: a create under a new request id uses it, and so does a replacement Session. A configuration change between a lost create reply and the next send does not fail the message: the send replays the create under the old request id with the new Agent, kagent answers `AlreadyExists`, and the turn treats that like a deleted request id and replaces it, so the message completes on a new Session on the renamed Agent. The Session that landed on the old Agent is then an orphan: every Agent a create was ever sent to is kept (`kagent_agents`, written with the marker and with a workspace), so the orphan sweep deletes it.
- The main session cannot be archived (a database trigger refuses it). At most one main binding exists in the whole database; the owner is a single user for now, so the index is global.
- A task that finished while no process was watching is finalized from `GetTask` on restart. A task parked for input keeps the turn open.
- Failure reasons are stored with best-effort redaction and a 300 character cut (see [chat](chat.md)).
- Routes (G6b), all owner-only and all on the same ledger: `POST /sessions` (create), `GET /sessions` (the owner's sessions except the main thread, newest first), `GET /sessions/{id}`, `GET /sessions/{id}/conversation`, `GET /sessions/{id}/native`, `POST /sessions/{id}/message`, `POST /sessions/{id}/stop-turn`, `POST /sessions/{id}/cancel`, `POST /sessions/{id}/archive`. Another owner's session and an unknown id are both `404`. `needs_owner` is the attention visibility rule ([inbox](inbox.md)): true while the session has a visible attention item. The list reads it for every session in one query, the detail and conversation routes read it for the one session, and a session just created reads false; its status stays the session's own (only the inbox snapshot reports `waiting_on_user`); the task-backed (delegated) cancel path of the Python backend is not ported.
- `POST /sessions/{id}/message` records the message and its delivery exactly like `POST /chat`, with one open turn per session: `409` while a delivery is `recorded`, `sending` or `delivered` or an `uncertain` one is inside its hold. It is also `409` for a cancelled or failed session, an archived one, one without a native binding, and a workspace whose Session does not exist yet.
- Stop turn cancels the A2A task with `CancelTask` (the kagent client and `kagenttest` both have it). It takes no lock (cancel is the one that takes the per-session send lock); the one-open-turn index and the fenced writes keep it consistent with a send. A delivery still `recorded` is cancelled without any kagent call. The task is cancelled outside any transaction; the delivery ends `cancelled`, the queue is held, and the partial reply and the stop note go into one message in one transaction (the note's id is derived from the delivery, so the delivery loop and the stop call settle the same turn once). Results: `stopped`, `finished` (the task had already ended, and its real outcome is recorded), `no_open_turn`. A task that is not visible yet or still running after the cancel is `409` and nothing changes; a kagent error is `502`. An `uncertain` delivery counts as open while it is inside its hold; one past the hold is skipped, since it no longer blocks sends and the delivery loop fails it, so it cannot mask a newer running turn (the Python backend looked only at recorded, sending and delivered). The stop is not repeated blindly.
- Cancel closes the session's open deliveries and then sets the sticky `cancelled` status in one transaction (deliveries before the session, the order a settle locks in; a session that already finished is left as it is), publishing one `session:message` event per closed delivery, and then cancels their tasks. The response carries the new `status` and `agent`: `stopped`, `not_running` (nothing had been sent), or `unknown` (kagent did not confirm; the agent may still be running and the cancel is not retried). The main thread cannot be cancelled (`409`). A cancelled or failed session takes no more messages.
- Archive clears one finished session from the list. A live session must be cancelled first (`409`, also when the session stopped being finished before the write), and the main thread is never archived (`409`). For a workspace the archived Session is deleted by the workspace service.
- `POST /sessions` creates a native session with its first prompt as a recorded delivery and answers with the session row; the first delivery creates the kagent Session.
- MCP credentials (G6c): the main thread's binding has grant `coordination`, and its first delivery publishes the binding's MCP credential before the kagent Session is created, sending it as the create's one credential reference ([credentials](credentials.md)). Sessions created by `POST /sessions` keep grant `none` and carry none. If the binding's previous Secret is still being deleted, the delivery waits and retries; those waits do not count against the create budget. Failed creates are counted per delivery in `deliveries.create_failures` (five failures settle it as failed), separately from claims (`attempts`), which also count credential waits and lease recoveries. Cancel revokes the session's credentials in the same transaction as the status change, and archive in the same transaction as the archive. Outside a cluster (no Secret store) the main thread is created without a credential and logs a warning. Cancel and archive end the binding for good: any future un-cancel or un-archive must re-`Issue` the binding's credential (the revoked one is not restored), and its create replays must then carry the new reference.

## Session list

At widths of 1024px and above, sessions appear in a sidebar beside the conversation.
Narrower windows use Chat, Sessions and Inbox tabs so the panels do not obscure the task or
session view. Task detail scrolls within the available pane; workspace controls wrap when needed.

The list defaults to active/pending work and sessions with server-projected owner attention,
including standalone sessions created on `/agents` and delegated child sessions. The native
main thread and archived sessions are excluded. **Finished (N)** reveals completed,
cancelled, failed and superseded sessions on desktop and mobile; the button shows as pressed
while they are visible, and pressing it again hides them. **Idle (N)** exposes available idle
sessions, with the same pressed state. These filters retain history and do not archive sessions.
The ambiguous bulk **clear** action is removed. With no current work, the list says
**No active sessions** and points to **+ agent** or delegation.

Each session shows its title, native runtime kind, and status. The server derives owner
attention from the same durable snapshot as the inbox. Task finality and attempt ownership
override stale session flags: a terminal task shows its outcome, and an old attempt shows
**SUPERSEDED**. Neither can show **NEEDS INPUT**. Workspace health appears separately.

| Status          | Meaning                                              |
| --------------- | ---------------------------------------------------- |
| pending         | Created but not yet active                           |
| active          | A native turn is in flight                           |
| idle            | Available for another message; no owner input needed |
| waiting_on_user | A current live native request genuinely needs input  |
| superseded      | An earlier or retired attempt, available as history  |
| completed       | Finished successfully                                |
| failed          | An error occurred                                    |
| cancelled       | Stopped by the user                                  |

Cancelled and failed are final. Agent activity does not change those statuses. A delegated
report remains an unverified claim until the task's own completion rules are satisfied.

Explicit owner-decision blockers also set `needs_owner`, independently of activity; these
sessions display **NEEDS YOU**. Idleness alone never sets it. Session reads and list filters
use the server projection; SSE prompts a fresh read instead of copying raw activity flags.
An idle child with no report and an idle standalone agent stay `idle`, including when their
previous activity status was `waiting_on_user`. Live owner requests are projected separately.
Both session detail and list reads scope that projection to the configured owner and ignore
`X-User-ID`; another owner's session returns `404` and never appears in the list.

The list includes all active and needs-you sessions plus the latest 100 unarchived history
records. History and idle toggle counts describe that returned window. Older sessions remain
available by direct session link. Zoom mode consumes the same current projection for its
status and attention banner, including settlement, task completion and reassignment. If a
zoomed session disappears from the list, it reads that session directly; it does not reuse
the conversation's old attention flag. An unavailable session disables message input.

## Creating and messaging sessions

- `/agents` offers Claude Code and Codex. `POST /sessions` accepts a configured provider profile ID or alias in `agent_kind`; when omitted, it defaults to Claude Code. See [provider profiles](providers.md).
- Each native session maps to one kagent Session (created on first use, resumed if suspended) on the configured kagent Agent for its kind. Mainloop does not create a Claude SDK worker for each session.
- The ordinary `/agents` creation path creates an ungranted `agent` binding. The separate
  validated `POST /workspaces` path enrolls a new owner workspace with a per-session MCP grant
  and stored project/repository/branch scope. Existing workspace bindings are not enrolled by
  migration; see [workspaces](workspaces.md) and [agent credentials](credentials.md).
- If kagent has deleted that Session (its idle TTL, or out of band), the next message creates a new one under a fresh request id and sends the standing context again. The provider's earlier context is gone; Mainloop's conversation history is kept. Turns still open on the deleted Session become `uncertain`. A `failed` kagent Session is reported, not replaced.
- Each user message is recorded with a delivery state before it is sent. Delivery states include `queued`, `recorded`, `sending`, `delivered`, `completed`, `failed`, `cancelled`, and `uncertain`. kagent allows one non-quiescent task per Session, so Mainloop queues report messages itself. A task waiting for input (`input-required`) stays `delivered` and blocks further turns until it is answered or the turn is stopped; the shared HITL card can answer it when the server permits owner responses.
- A message still `recorded` after a backend restart was never sent, so it is delivered then; this is its first send, not a replay. A `sending` message with no task after 60 seconds, including when the lookup itself keeps failing, becomes `uncertain`.
- A failed or uncertain delivery stores a short reason with best-effort redaction (see [chat](chat.md)). The session chat shows it next to the message and the identity strip shows the last problem, and the same Retry rule applies: a new message with a new id, offered only for the newest failed user message with nothing in flight.
- An uncertain delivery is never replayed automatically. A message is rejected with `409` while another turn is in flight. A message for a suspended workspace resumes it first; one that arrives while the workspace is being suspended waits for the suspend to finish, then resumes it.
- Session conversations mirror the user's messages and each completed task's reply. The reply id is derived from the task id, so a repeated observation mirrors it once.

## Cancelling and clearing

- Cancel ends the session and cancels its open A2A tasks. If Mainloop cannot confirm the stop, it reports that result and does not repeat the stop blindly.
- A cancelled session no longer accepts messages.
- Stop turn (`POST /sessions/{id}/stop-turn`, owner only) cancels the open A2A task with CancelTask and keeps the session and its kagent Session, so a parked (`input-required`, `auth-required`) or runaway turn can be cleared. It works for the main thread and for every other native session. The main thread has a `stop` button next to its working indicator; every session detail page offers **Stop turn** independently of Cancel session, except archived, failed and cancelled sessions, where it is hidden.
  - The delivery ends as `cancelled`, the queue is held, and the conversation gets the partial reply followed by "This turn was stopped before it finished.", in one transaction. Streamed text is retained durably even if the cancellation response omits artifacts. The reply is visibly marked as stopped. Cancellation is recorded only after kagent confirms it (or before sending a still-recorded message).
  - Queued reports stay queued across refreshes and restarts. The owner's next explicit message releases the hold and starts a new task on the same Session, ahead of the held reports. The reports follow once that turn finishes. A report alone never releases the hold.
  - Stopping with no open turn changes nothing (`no_open_turn`). If the turn completed or failed first, that outcome is recorded and the response is `finished`.
  - A kagent error returns `502` and leaves the delivery as it was; a send with no visible task yet, or a task that is still running after the cancel, returns `409`. Neither is retried automatically.
  - A task observed as cancelled from any other source (a stream, a sync) also ends the delivery as `cancelled`, with the same partial text, note and queue hold.
- The archive API retains finished sessions for audit. Live sessions must be cancelled first.
  The list's history toggle does not archive or delete sessions.
- Task cancellation uses the durable task controls; native session cancellation remains available
  where the current attempt's authority permits it.

## Session detail

Assistant message prompts and processing indicators use the native runtime kind (`claude` or `codex`), including the main thread, inline replies, and fullscreen session views. Unknown or missing identity is labelled `agent` rather than guessed from a model name.

The session view shows the conversation, session status, and a native identity strip with the agent kind, model, kagent Agent and Session state, turn count, delivery states, and the reason for the last failed or unconfirmed delivery. Workspace health and lifecycle controls are shown separately.

The owner-only `GET /sessions/{id}/native` (and the main thread's `native`) also returns an optional
`preparation_observation`, projected from the kagent GetSession response it already reads for the
Session state: one GetSession, no second RPC. It includes Session/context identity and whether the
context was explicit, state/operation and prepared revision, the observed runtime generation/Actor UID,
and allowlisted original preparation action/create/generation identity, setup profile/digest,
classification, historical flag, challenge/sequence and effect/observation timestamps. Timestamps
retain protobuf seconds/nanoseconds. Absent optional messages and empty identity fields stay
unavailable; proto3 scalar defaults are retained without claiming field presence. `receipt_status`
distinguishes `observed`, `missing` and `unavailable` (a receipt that cannot be safely projected,
including a foreign identity or a value outside the contract's bounds). Runtime association and
receipt are independently optional. The observation itself is absent, and with it `session_state`
(the note says "kagent session unavailable"), when GetSession is unavailable or its reply is for
another Session or context, repeats the workspace preparation or the runtime association on the wire,
carries a receipt that names no original request, or has runtime fields that cannot be projected
(an unknown state or operation, an identity outside its bounds). The duplicate check runs on the raw
reply before protobuf merges the copies, in the kagent client's session codec, and also refuses a
repeated singular field inside the receipt; the call then has an unknown outcome, as in the Python
client. A Session that states no context id is shown with its own id as the context and
`context_confirmed: false`, read through a display-only client call (`ObserveSession`); delivery,
lifecycle and readiness keep requiring a confirmed context, and a context naming another Session is
refused either way.
The response excludes credentials, configuration documents, checkout content and arbitrary runtime
data: only the fields in `NativePreparationObservation` of `api/openapi.yaml` are ever copied. This
diagnostic API does not add a UI flow or a second RPC, refresh preparation, start work, or grant
dispatch authority. Runtime `ready` alone does not prove preparation; saved receipt evidence does
not identify an earlier runtime admission failure's cause. The `web/` client has the generated types
only; no screen shows the observation yet.

Opening a session follows its URL. Missing sessions show a not-found state. If the backend is unavailable, the page retries instead of treating the session as missing.
An HTTP 5xx on session or workspace detail shows a server error with an instruction to reload,
separately from a failed connection or a 404.

## Evidence boundary

Measured on the kagent spike cluster (live, 2026-10-04): re-sending a `messageId` whose task had completed started a second task, so Mainloop relies on never re-sending, not on kagent deduplication. The kagent client and delivery handling are tested against a fake A2A and SessionService gateway with sanitized fixtures (`backend/tests/runtime/fixtures/kagent`). These are fixture tests, not live proof. The Kind session-resume proof in `docs/spikes/k8s-herdr-agents.md` is historical and does not prove the kagent path.

## Observed sessions needing input

The backend also observes gateway-owned standalone sessions without creating a Mainloop
binding, project assignment, MCP credential, or delivery row. Their pending input can create
an inbox reference through the [HITL observer](merge-policy.md#background-discovery-and-structured-continuation).
Ownership is verified before reading task contents; unavailable ownership or continuation
mapping cannot enable answers. The observer does not start or resume sessions.

Session chats list up to 100 current request projections, newest observation first, independently
of ordinary deliveries. Repeated observations do not duplicate a card. Successive requests
replace superseded controls; recorded responses remain read-only. The visibility-aware UI refresh
reads existing observations and does not start Actors. Unbound observed sessions remain in the
inbox and are not assigned a synthetic conversation or project. See [chat](chat.md#native-structured-input).

## Delegated task sessions

Task attempts pin native provider revision and role AgentRef. A task supervisor (depth 1) and
its direct child (depth 2) have independent native identities; no provider history, filesystem or
credentials are inherited. Lost create replies reconcile the same persisted request. A deleted
runtime is not recreated in place for a delegated attempt; retry or handoff needs a successor
attempt through the task lifecycle. The ordinary owner-session replacement behavior above remains.

Finishing a native turn records delivery/activity and the mirrored response, not task completion.
Task-backed children do not invoke the session-child automatic completion report. Cancelling a
delegated session goes through task cancellation and confirmed runtime fencing; an uncertain
fence keeps writer and capacity reservations. Clearing checks the attempt before archiving on the
same locked database connection; a terminal session badge alone is insufficient. Superseded
attempts cannot take this immediate-delete clearing path.

Fresh cutover deletes old sessions; there is no old-shape migration, legacy workspace enrollment,
or transcript conversion. The task-session checks have fake runtime and PostgreSQL evidence only;
there is no live native-agent, Kubernetes or retained-source handoff qualification in this slice.

## React status

Labels: **implemented** means built in `web/` and covered by component or route tests against a fake backend. Nothing here is live-proven.

- **Implemented:** the session list, built from attention, with the finished and idle filters and their counts; session rows; the `/agents` form (Claude Code or Codex, Ctrl/Cmd+Enter submits, blocked while offline); and the session page. The page offers **Stop turn** except on archived, failed and cancelled sessions, **Cancel session** through an in-app confirmation (never `window.confirm`), and **Clear** for completed, failed and cancelled sessions, which returns to the list. It also shows the task link, workspace badges, summary or error, the identity strip, and the not-found, server error and unreachable states.
- **Implemented:** after any session action, the page re-reads the session, its native state and attention before it shows the result. A `409` therefore shows the current state.
- **Implemented:** when a list refresh fails after the list has loaded, the list keeps its last rows and shows a **Retry loading sessions** line. Only a failed first load replaces the list with an error.
- **Implemented:** while the list or the session page first loads, placeholder rows with a row's height sit under the loading text. The text is what a screen reader announces; the placeholders are hidden from it.
- **Wording:** the identity strip's live word is the agent's own state, shown verbatim. It shows `live` when the agent is ready, otherwise whatever state string the agent reports (for example `busy` or `suspended`, and any other word it sends), or `unknown` when there is no state yet. A failed or unconfirmed last delivery takes priority and shows as `last message failed` or `last message unconfirmed`, so it never reads `live`. When the backend cannot be reached the word is `unreachable`, which the page adds itself; an agent never reports it.
- **Implemented:** the session chat in the page's chat slot (`SessionChatSlot`). It shows the session's own conversation, with the same timeline, delivery notices, retry and composer as the main thread. It reads `GET /sessions/{id}/native` and `GET /sessions/{id}/conversation`, and sends to `POST /sessions/{id}/message`. Its unsent draft is kept for that session only. While a turn is in flight the header shows the turn's state and a **stop** button. It does not show pending requests or the topic index.
- **Implemented:** a closed session (archived, failed or cancelled) locks its chat composer and says why in the chat panel: "This session is archived, so it takes no new messages.", or the same for failed or cancelled. **Stop** is hidden there too. A completed session stays open. The panel is sized to 70% of the visible viewport, so it shrinks when a soft keyboard opens. The visible height is the visual viewport's height times its scale, so pinch-zooming in does not shrink it; a browser without a visual viewport uses 70% of the dynamic viewport height instead.
- **Unreachable:** the page recovers in two ways. When the connection comes back, the shared live-updates module (`web/src/shell/LiveUpdates.tsx`) refetches open queries without a click. The state also offers a manual **Try again** button.
- **Not built here:** the notification toast.
- **Not proven:** the native poll while a turn is in flight (not cheap to test with MSW), and any run against a live backend, the session chat included. The panel's soft-keyboard sizing is tested with a mocked visual viewport only; it has not been checked on an iOS device or at phone width.
