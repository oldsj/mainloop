# Sessions

Sessions are background AI work spawned from the main thread. Each session has its own conversation and runs independently.

## Session List

Desktop shows sessions in a sidebar. Mobile shows sessions in a tab.

When no sessions exist:

- Shows empty state with "No sessions yet" message
- Shows hint: "Sessions appear when Claude spawns background work"

When sessions exist:

- Each session shows title and status badge
- Active count shown in header (e.g., "2 active")
- Clicking a session opens its detail view

## Status Badges

| Status          | Badge       | Meaning                  |
| --------------- | ----------- | ------------------------ |
| pending         | PENDING     | Queued, not started      |
| active          | ACTIVE      | Currently running        |
| waiting_on_user | NEEDS INPUT | Blocked on user response |
| completed       | DONE        | Finished successfully    |
| failed          | FAILED      | Error occurred           |

Failed sessions show error message below the badge.

## Session Detail View

Clicking a session navigates to `/sessions/{id}`:

- Shows title as h1 heading
- Shows description if present
- Shows the session's chat directly (there is no Logs tab)
- Shows a one-line identity summary (agent, model, live or idle, topic) that expands to the full identity strip
- Follows the URL: opening another session from the list switches to it
- The session open in the main pane is highlighted in the list
- Active sessions show Cancel button
- Completed sessions show Summary section
- Failed sessions show Error section
- Back button returns to home
- Non-existent session ID shows "Session not found" with link to home
- When the backend is unreachable the page says so and retries when it returns, instead of "Session not found"

## Notifications

When a session needs attention:

- Toast notification appears with title and preview
- Clicking notification navigates to that session's detail view

## Native Agent Sessions (implemented in the local kind slice; not production)

`/agents` ("new agent session" in the header, "+ agent" in the session list) starts a session bound to a real
agent instead of the session worker:

- Choose **Claude Code** or **Codex**, an optional title and a first message. The session is created with
  `agent_kind` (`POST /sessions`); no Job or namespace is created.
- The agent runs under Herdr in the workspace pod in bypass-permissions mode. The approval policy is recorded on
  the binding and shown in the UI.
- The session detail view shows an identity strip: agent kind, model (read from the native journal), approval
  policy, native session id, Herdr pane, workspace pod (short UID, ready or not), whether the agent process is
  live, the ownership generation, the journal file and cursor, and the state of each delivery.
- Delivery states: `recorded`, `sending`, `delivered`, `completed`, `failed` (nothing sent), `uncertain`
  ("delivery unknown"). A prompt is sent once. If the outcome is unknown the UI says so and waits for the user;
  it never resends automatically. While a turn is in flight a second message is rejected (`409`).
- Replacing the pod keeps the conversation: the agent is shown as "not running (resumes on next message)"; the next
  message restarts it with the native resume flag against the same native session id (generation increases) and
  then delivers the message.

Measured with real agents (Claude Code 2.1.278, codex-cli 0.155.1) on the local kind cluster only; see
`docs/spikes/k8s-herdr-agents.md`. Known gaps: the header status badge is loaded once, message text is rendered
as markup (angle brackets in messages disappear), and there is no way to mark an `uncertain` delivery resolved.

## Delegated child sessions (implemented in the local kind slice; flag `MAIN_THREAD_MODE=native`)

A session started by the native main thread (`mainloop delegate`) is a child: it has a parent (the main thread),
a topic, and runs as a native Claude Code or Codex agent in its own scratch directory under Herdr in `workspace-0`.

- The session list marks children with `↳` and `#<topic>`; the identity strip shows role, parent and topic.
- The child receives one task brief (a ledgered delivery with source `brief`); Mainloop never sends it the
  parent's transcript.
- The child ends with `mainloop report --summary` (size-capped). The report is recorded on the topic as evidence
  and delivered to the main thread. If a turn ends without a report, its last reply is reported with a
  "fallback" label.
- You can still message a child directly from its session view; that is an ordinary ledgered delivery.
- The main thread's own binding is not listed as a session; it is the home conversation.
