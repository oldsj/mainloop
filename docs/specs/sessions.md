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
- Native CLI execution lives in the Substrate actor selected by the configured provider binding. Mainloop's credential broker owns real provider credentials in control-side Secrets; actors receive synthetic placeholder files only. Mainloop does not create a Claude SDK worker for each session.
- Each user message is recorded with a delivery state before it is sent. Delivery states include `recorded`, `sending`, `delivered`, `completed`, `queued`, `failed`, and `uncertain`.
- An uncertain delivery is never replayed automatically. A message is rejected with `409` while another turn is in flight or while the workspace is suspending or suspended.
- Session conversations mirror messages and turn evidence from the native journal.

## Cancelling and clearing

- Cancel ends the session and asks the actor to stop the native turn. If Mainloop cannot confirm the stop, it reports that result and does not repeat the stop blindly.
- A cancelled session no longer accepts messages.
- Clear archives finished sessions for audit. Live sessions must be cancelled first.
- The main thread can cancel or clear its child sessions through the `mainloop` tools.

## Session detail

The session view shows the conversation, session status, and a native identity strip with the agent kind, model, approval policy, native session id, configured workspace actor, readiness, generation, journal cursor, and delivery states. Workspace health and lifecycle controls are shown separately.

Opening a session follows its URL. Missing sessions show a not-found state. If the backend is unavailable, the page retries instead of treating the session as missing.

## Evidence boundary

Native journal parsing, delivery handling, and Substrate transport tests use sanitized fixtures and fake routers. The earlier Kind session-resume proof is retained as historical evidence in `docs/spikes/k8s-herdr-agents.md`; it does not prove the current Substrate runtime.
