# Sessions

Sessions are native Claude Code or Codex work started from the home thread or the `/agents` page. Mainloop keeps the product session, workspace lifecycle, native binding, and message delivery as separate records.

## Session list

At widths of 1024px and above, sessions appear in a sidebar beside the conversation.
Narrower windows use Chat, Sessions and Inbox tabs so the panels do not obscure the task or
session view. Task detail scrolls within the available pane; workspace controls wrap when needed.

The list defaults to active/pending work and sessions with server-projected owner attention,
including standalone sessions created on `/agents` and delegated child sessions. The native
main thread and archived sessions are excluded. **Show finished (N)** reveals completed,
cancelled, failed and superseded sessions on desktop and mobile; **Hide finished (N)** hides
them again. **Show idle (N)** exposes available idle sessions. These filters retain history and
do not archive sessions. The ambiguous bulk **clear** action is removed. With no current work,
the list says **No active sessions** and points to **+ agent** or delegation.

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
- Stop turn (`POST /sessions/{id}/stop-turn`, owner only) cancels the open A2A task with CancelTask and keeps the session and its kagent Session, so a parked (`input-required`, `auth-required`) or runaway turn can be cleared. It works for the main thread and for every other native session. The main thread has a `stop` button next to its working indicator; every session detail page offers **Stop turn** independently of Cancel session.
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

The owner-only `GET /sessions/{id}/native` also returns an optional
`preparation_observation`, projected from its existing authenticated kagent GetSession
response. It includes Session/context identity and whether the context was explicit,
state/operation and prepared revision, the observed runtime generation/Actor UID, and
allowlisted original preparation action/create/generation identity, setup profile/digest,
classification, historical flag, challenge/sequence and effect/observation timestamps.
Timestamps retain protobuf seconds/nanoseconds. Absent optional messages and empty identity
fields stay unavailable; proto3 scalar defaults are retained without claiming field presence.
`receipt_status` distinguishes `observed`, `missing` and `unavailable` (a receipt that
cannot be safely projected, including a foreign identity). Runtime association and receipt
are independently optional. The observation itself is absent when GetSession is unavailable
or its runtime fields cannot be safely projected.
The response excludes credentials, configuration documents, checkout content and arbitrary
runtime data. This diagnostic API does not add a UI flow or a second RPC, refresh preparation,
start work, or grant dispatch authority. Runtime `ready` alone does not prove preparation;
saved receipt evidence does not identify an earlier runtime admission failure's cause.

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
