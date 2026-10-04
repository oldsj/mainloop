# Chat

The home chat is the user's native Claude Code main session, run by a kagent Agent. The provider owns native session history and tools; Mainloop records logical messages and delivery state.

## Sending messages

- The input field submits one user message to the main session.
- Mainloop records the conversation message and delivery intent before contacting kagent. The message id is the A2A `messageId`.
- A prompt is sent once. If kagent reports it did not accept the message, Mainloop retries the same message for up to 30 seconds, then marks the delivery failed (not sent); a failed delivery is not requeued. If the outcome is otherwise unknown, Mainloop looks for the task by message id; if none shows it, the delivery is marked uncertain and is never replayed.
- The response is the completed A2A task's text, mirrored into the conversation once. While the page is open it polls for the mirrored reply.
- A second message is rejected with `409` while a delivery is in flight.

## Conversation history

- User and assistant messages persist across page reloads.
- The native session remains authoritative for provider history and context management; Mainloop mirrors observed messages and delivery receipts.
- The main session keeps one kagent Session until kagent deletes it (for example after its idle TTL); then the next message starts a new one, see Sessions. Context length is managed by the provider's native auto-compaction, which is configured per harness outside Mainloop; Mainloop does not rotate the session or ask it to write out state. Standing context is sent with the first message to each kagent Session.

## Delegating sessions

- The main session can create Claude Code or Codex child sessions through the `mainloop` tool.
- Child reports are stored against their topic and delivered to the main session as ledgered messages. Reports arriving during another turn are queued.
- The main session can inspect status and stored reports without sending a prompt to a child.

## Identity and policy

The identity strip shows the native agent, the kagent Agent and Session with its runtime state, model, turn count, and delivery states. Mainloop's per-binding token scopes its tool commands but is not a security boundary; backend API authorization and isolation remain separate concerns.
