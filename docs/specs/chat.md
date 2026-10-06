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

- The main session can create Claude Code or Codex child sessions through the `delegate` tool on the `mainloop` MCP server.
- Child reports are stored against their topic and delivered to the main session as ledgered messages. Reports arriving during another turn are queued.
- The main session can inspect status and stored reports without sending a prompt to a child.

## Identity and policy

The identity strip shows the native agent, the kagent Agent and Session with its runtime state, model, turn count, and delivery states. Tool identity comes from a per-binding credential injected by the Substrate egress gateway;
the native agent holds only a placeholder. MCP discovery and calls are filtered by role:
the main thread manages topics, pending intent and its child tree; children can call `whoami`,
`note`, `decide` and `report`, and cannot delegate or inspect siblings. Inputs are validated
and policy failures return tool errors. Terminal and archived bindings cannot authenticate.
The dedicated MCP origin exposes only `/mcp`. REST remains unauthenticated and relies on the
required NetworkPolicy isolation documented in the architecture guide. The channel is
implemented with fake-backed tests; the joint gateway proof (native agent to MCP through the gateway,
blocked connections) passed on a Cilium cluster and needs a NetworkPolicy-enforcing CNI to repeat.

## Initial installation and history boundary

Slice a2 starts from a fresh database; upgrades from earlier slices are unsupported. Dev/spike
data must be reset rather than migrated. Every main/child Session is created with its credential
reference from the start, and the main thread uses the dedicated main Agent. There is no
pre-a2 cutover or credential-less binding path, and earlier native session history is not
carried into a2.

A definitively failed initial child start is terminal and revokes tool identity through durable
Secret cleanup. An uncertain creation/readiness/deletion outcome is reconciled before startup
is abandoned; the brief remains unsent, and Mainloop never starts a second writer to recover it.
Pending creation is retried with the persisted creation request ID, and pending disposal with
the existing native Session ID. Observation alone does not advance these lifecycle operations.
Identity is revoked only after confirmed absence or settled deletion; unrelated pending
lifecycle operations defer startup disposal. A durable disposal intent and the brief's send
claim are mutually exclusive across control-plane processes.
Failures after creation/readiness, including a standing-context read before the brief's send
claim, follow the same durable disposal path. They leave the brief unsent and the child
nonterminal until the reserved actor is confirmed absent or disposed. Only a proven
pre-reservation create rejection can fail startup directly.
A failed main-thread delivery remains recoverable independently of child startup failure.
