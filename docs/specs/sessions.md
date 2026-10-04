# Sessions

Sessions are native Claude Code or Codex work started from the home thread or the `/agents` page. Mainloop keeps the product session, workspace lifecycle, native binding, and message delivery as separate records.

## Session list

Desktop shows sessions in a sidebar. Mobile shows sessions in a tab.

When no sessions exist, the list explains that sessions appear when work is delegated or started. Each session shows its title and status. Workspace health and controls appear separately from session status.

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

- `/agents` offers Claude Code and Codex. `POST /sessions` accepts `agent_kind`; when omitted, it defaults to Claude Code.
- Each native session maps to one kagent Session (created on first use, resumed if suspended) on the configured kagent Agent for its kind. Mainloop does not create a Claude SDK worker for each session.
- If kagent has deleted that Session (its idle TTL, or out of band), the next message creates a new one under a fresh request id and sends the standing context again. The provider's earlier context is gone; Mainloop's conversation history is kept. Turns still open on the deleted Session become `uncertain`. A `failed` kagent Session is reported, not replaced.
- Each user message is recorded with a delivery state before it is sent. Delivery states include `queued`, `recorded`, `sending`, `delivered`, `completed`, `failed`, and `uncertain`. kagent allows one non-quiescent task per Session, so Mainloop queues report messages itself. A task waiting for input (`input-required`) stays `delivered` and blocks further turns until it is answered or the session is cancelled; answering it is not yet supported.
- A message still `recorded` after a backend restart was never sent, so it is delivered then; this is its first send, not a replay. A `sending` message with no task after 60 seconds, including when the lookup itself keeps failing, becomes `uncertain`.
- An uncertain delivery is never replayed automatically. A message is rejected with `409` while another turn is in flight or while the workspace is suspending or suspended.
- Session conversations mirror the user's messages and each completed task's reply. The reply id is derived from the task id, so a repeated observation mirrors it once.

## Cancelling and clearing

- Cancel ends the session and cancels its open A2A tasks. If Mainloop cannot confirm the stop, it reports that result and does not repeat the stop blindly.
- A cancelled session no longer accepts messages.
- Clear archives finished sessions for audit. Live sessions must be cancelled first.
- The main thread can cancel or clear its child sessions through the `mainloop` tools.

## Session detail

The session view shows the conversation, session status, and a native identity strip with the agent kind, model, kagent Agent and Session state, turn count, and delivery states. Workspace health and lifecycle controls are shown separately.

Opening a session follows its URL. Missing sessions show a not-found state. If the backend is unavailable, the page retries instead of treating the session as missing.

## Evidence boundary

Measured on the kagent spike cluster (live, 2026-10-04): re-sending a `messageId` whose task had completed started a second task, so Mainloop relies on never re-sending, not on kagent deduplication. The kagent client and delivery handling are tested against a fake A2A and SessionService gateway with sanitized fixtures (`backend/tests/runtime/fixtures/kagent`). These are fixture tests, not live proof. The Kind session-resume proof in `docs/spikes/k8s-herdr-agents.md` is historical and does not prove the kagent path.
