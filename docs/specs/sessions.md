# Sessions

Sessions are native Claude Code or Codex work started from the home thread or the `/agents` page. Mainloop keeps the product session, workspace lifecycle, native binding, and message delivery as separate records.

## Session list

Desktop shows sessions in a sidebar. Mobile shows sessions in a tab.

The list includes standalone sessions created on `/agents` and delegated child sessions, excluding the native main thread and archived sessions. Starting an agent adds it to the list immediately. When no sessions exist, the list points to **+ agent** or delegating work from the home thread. Each session shows its title, native runtime kind (Claude or Codex), and status. Workspace health and controls appear separately from session status.

| Status          | Meaning                                           |
| --------------- | ------------------------------------------------- |
| pending         | Created but not yet active                        |
| active          | A native turn is in flight                        |
| waiting_on_user | The agent is idle and can receive another message |
| completed       | Finished successfully                             |
| failed          | An error occurred                                 |
| cancelled       | Stopped by the user                               |

Cancelled and failed are final. Agent activity does not change those statuses. A child that has reported is completed; if the user sends another message, it becomes active until that turn finishes.

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
- Clear archives finished sessions for audit. Live sessions must be cancelled first.
- The main thread can cancel or clear its child sessions through the `mainloop` tools.

## Session detail

Assistant message prompts and processing indicators use the native runtime kind (`claude` or `codex`), including the main thread, inline replies, and fullscreen session views. Unknown or missing identity is labelled `agent` rather than guessed from a model name.

The session view shows the conversation, session status, and a native identity strip with the agent kind, model, kagent Agent and Session state, turn count, delivery states, and the reason for the last failed or unconfirmed delivery. Workspace health and lifecycle controls are shown separately.

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
