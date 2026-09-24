# Chat

The home chat is the user's native Claude Code main session in a configured Substrate actor. The provider owns native session history and tools; Mainloop records logical messages and delivery state.

## Sending messages

- The input field submits one user message to the main session.
- Mainloop records the conversation message and delivery intent before contacting the actor.
- A prompt is sent once. If the transport outcome is unknown, Mainloop marks the delivery uncertain and does not replay it.
- The response is mirrored from the native journal into the conversation. While the page is open, it polls for new journal evidence and turn completion.
- A second message is rejected with `409` while a delivery or rotation is in flight.

## Conversation history

- User and assistant messages persist across page reloads.
- The native session remains authoritative for provider history and context management; Mainloop mirrors observed messages and delivery receipts.
- Mainloop rotates the main native session after its configured token-growth or turn budget. Before rotation, it asks the current session to write durable state through the allowed Mainloop tools, then starts a new native session with a generated startup context.

## Delegating sessions

- The main session can create Claude Code or Codex child sessions through the `mainloop` tool.
- Child reports are stored against their topic and delivered to the main session as ledgered messages. Reports arriving during another turn are queued.
- The main session can inspect status and stored reports without sending a prompt to a child.

## Identity and policy

The identity strip shows the native agent, model, approval policy, native session id, configured workspace actor and readiness, delivery generation, rotation counters, journal cursor, and delivery states. Mainloop's per-binding token scopes its tool commands but is not a security boundary; backend API authorization and isolation remain separate concerns.
