# Chat

The home chat is the owner's native Claude Code or Codex main session, run by a matching kagent Agent. Claude is the default. The provider owns native session history and tools; Mainloop records logical messages and delivery state.

## Selecting the main provider

`MAIN_PROVIDER=claude|codex` selects the first-use default. Existing main bindings keep
their persisted selection across restarts and configuration changes. The owner can read
`GET /main-thread/provider` and explicitly select a provider with
`PUT /main-thread/provider` and `{"provider":"codex"}` (or `"claude"`). These routes use
the existing owner API boundary and cross-origin write guard; MCP agents cannot select
the main provider. The main-thread header exposes an owner-only Main provider dropdown. There is no quota-driven fallback.

The dropdown separates desired selection, confirmed effective provider and preparation status.
It is disabled offline, during selection, loading, sending or stopping, while a turn or
recorded/sending/delivered/uncertain delivery is open, and while selection is pending.
Selecting the same ready provider does nothing. A failed selection offers explicit retry;
a recoverable failure offers explicit restart with `restart_failed=true`. Neither runs automatically.
A conflict is displayed and state refreshed. Network failures reconcile through GET without
repeating PUT; an unsuccessful read keeps main input and selection locked until polling
confirms state. Older poll results cannot overwrite selection results. Main input is disabled
while provider preparation is pending/failed or unknown, while drafts and child input remain
independent. A not-started binding can accept its first message.

The response exposes `desired_provider`, `effective_provider`, `status`, the current
`kagent_session_id`, a bounded `error`, and retained native identity `history`.
`recoverable` identifies a pending target confirmed absent or settled failed/deleted,
including a frozen create receipt that confirms its Session was deleted before an ID was saved.
`/main-thread` includes this provider state alongside its current native identity.
`not_started` means the default binding has not created a native Session. `pending`
means a switch contract is committed but preparation has not completed; `failed`
means its latest preparation failed. Both have a null effective provider. A pending
target may have a native Session ID before readiness is confirmed.

A switch requires the main to be idle, with no recorded, queued, sending, delivered or
uncertain deliveries, no active native task and no unsettled native lifecycle operation.
Failure to establish native idleness returns `409` without changing the selection.
The old native Session is suspended, preserved in the audit history and has its MCP
authority revoked. The new provider receives a new native identity and credential;
its create request and Agent reference are persisted before creation. The product
session, conversation, messages, topics, child task identities, parent references and
capacity remain unchanged. Provider-native history stays with its original provider;
only stored standing context and recent completed conversation messages carry over.
Failed and cancelled instructions are excluded from that carry-over.

Bootstrap reuses a main only while its product binding has active coordination authority.
An archived, revoked, deleted or terminal binding relinquishes current ownership in the
same transaction that creates its replacement. Concurrent requests share one replacement,
using the first-use provider default; existing native contracts and history remain on their
original bindings, and no prior delivery is replayed. Unsent managed reports can route to the
valid replacement; claimed or uncertain reports retain their original recipient receipts.
Historical main membership persists independently of role or current authority. Migration
retains that membership, releases inactive current-owner slots, backfills slots only for
valid mains and refuses competing valid owners. Provider operations cannot restart a revoked
retained contract, even after startup/schema replay: ordinary bootstrap must first establish
a valid replacement before a new provider selection. An inactive historical main does not
block selection or pinned recovery on that replacement. Provider selection on true first
use remains supported; ordinary workers and children do not count as historical mains.

Repeating the current selection is idempotent. Repeating a pending selection recovers
its same native create contract; it does not send a prompt. Chat returns `409` while
preparation remains pending, and arriving child reports are queued until readiness is
confirmed. After a failed request or restart, read status and repeat the same PUT to
recover. Another provider cannot replace an unresolved selection. There is no automatic
switch recovery loop; normal delivery reconciliation drains reports after preparation.
Recovery and idempotent selection use the persisted Agent and create contract even if
the current provider catalog disables, removes or changes its main entry. New selections
and fresh recovery identities validate the current catalog.

If a repeat observes the pending target as absent or settled failed/deleted, the failure
and `recoverable=true` are durable. The owner can then select another provider, or repeat
the current provider with `{"provider":"codex","restart_failed":true}` to admit a fresh
native and credential identity. The abandoned target remains in history. This operation
rechecks absence or settled failure and refuses active tasks or uncertain lifecycle work;
held child reports stay queued for the new target. A lost create reply or an unsettled
failure keeps the original create contract, including when `restart_failed=true` is sent.
No recovery operation replays a prompt.
If a suspend response is lost before the switch commits, the original provider remains
current and a repeated PUT first reconciles that same old native Session. No failed
delivery or old prompt is replayed by a provider selection.

Ordinary main MCP writes revalidate current credential authority and commit under the
same authority lock as retirement. An old request paused after authentication is refused
if the provider switch retires its generation first. A terminal delivery, its mirrored
reply and its turn accounting commit together before retirement can snapshot that generation.

Both main Agents must be installed with matching native harnesses and ModelConfigs,
MCP credential routing and `sessionIdleTTL: 0s`. Claude uses `KAGENT_MAIN_AGENT`;
Codex uses `KAGENT_MAIN_CODEX_AGENT`. Explicit provider profiles may override their main
Agent references. The repository's Kind main-Agent example includes both providers;
production installations must supply reviewed images and their existing provider
authentication through GitOps before selecting Codex. This behavior is covered by
offline fixtures and scratch PostgreSQL tests; a live switch remains a deployment check.

## Sending messages

- The input field submits one user message to the main session.
- Mainloop records the conversation message and delivery intent before contacting kagent. The message id is the A2A `messageId`.
- A prompt is sent once. If kagent reports it did not accept the message, Mainloop retries the same message for up to 30 seconds, then marks the delivery failed (not sent); a failed delivery is not requeued. If the outcome is otherwise unknown, Mainloop looks for the task by message id; if none shows it, the delivery is marked uncertain and is never replayed.
- The response is the completed A2A task's text, mirrored into the conversation once. While the page is open it polls for the mirrored reply.
- A second message is rejected with `409` while a delivery is in flight.
- A failed delivery keeps a short reason, stored with the delivery: the kagent/A2A error class and message (for example `not sent: SessionError: …`, `send rejected: A2AError (INVALID_PARAMS): …`, or `task failed: <kagent's message>`). Best-effort redaction covers credential assignments, authorization headers, bearer tokens, URL userinfo and long opaque tokens; arbitrary harness text is not guaranteed to be secret-free. Whitespace is collapsed, and the text is cut to 300 characters.
- The reason is shown on the message it belongs to, in a `Failed`/`Unconfirmed` notice. The header and identity strip show `last message failed` (red) or `last message unconfirmed` (yellow) instead of `ready` or `working`; a delivery still in flight takes precedence and shows `working`. The notice survives a reload because it is read from the stored delivery.
- A failed (not uncertain) user message offers **Retry** while it is the newest delivery and nothing is in flight. Retry checks fresh delivery state and always targets the owning conversation, even when a child session is selected. A local send guard stays held until submission and refresh finish, so polling cannot enable a second submission. Retry sends the same text as a new message with a new message id; the failed message and its id are never re-sent. An uncertain delivery is not retried, because the original may have been accepted.

## Conversation history

- User and assistant messages persist across page reloads.
- The native session remains authoritative for provider history and context management; Mainloop mirrors observed messages and delivery receipts.
- The main session keeps one kagent Session until kagent deletes it (for example after its idle TTL); then the next message starts a new one, see Sessions. Context length is managed by the provider's native auto-compaction, which is configured per harness outside Mainloop; Mainloop does not rotate the session or ask it to write out state. Standing context is sent with the first message to each kagent Session.

## Delegating tasks

- Main creates durable supervisor tasks through `delegate`; each supervisor may create direct children in its inherited project/tree. Children cannot delegate further.
- Explicit progress and result reports persist against their task attempt and queue notifications to the current authorized parent and main. Busy parents use the existing delivery ledger.
- `task_get`, `task_list` and `task_history` inspect stored progress without native turns. Task cancellation uses the shared service; retry/reassign remain unavailable until handoff is installed.
- Native turn completion creates no automatic report or task success. Coding success requires verified merged publication; coordination completion requires explicit success, no live children and confirmed runtime settlement.
- See [delegation](delegation.md) for inputs, scope and restart behavior.

## Identity and policy

The identity strip shows the native agent, the kagent Agent and Session with its runtime state, model, turn count, and delivery states. Tool identity comes from a per-binding credential reference injected by kagent's credential path; the native agent receives a placeholder, not the bearer. MCP discovery and calls use the binding role and persisted grant together. The main thread manages topics, pending intent and its durable task trees. Task supervisors and children can report their own current attempt and read scoped task history; children cannot delegate or inspect siblings. A newly created owner workspace can call `whoami` and `open_pull_request` within its stored project and branch scope. Merge tools remain behind the existing enablement gates. Ordinary owner workspace grants do not provide topic writes, reports or delegation. Delegated workspace grants additionally supply task tools within their persisted task scope; coordination grants never supply repository authority. Ordinary standalone and retained legacy workspace bindings stay ungranted. Inputs are validated and policy failures return tool errors. Terminal, archived and revoked bindings cannot authenticate.
The dedicated MCP origin exposes `/mcp` and the isolated `/mcp/merge-approval` tool surface; merge tools remain disabled pending the [merge enablement gates](pull-requests.md#enablement-remains-separate). REST remains unauthenticated and relies on the
required NetworkPolicy isolation documented in the architecture guide. The channel is
implemented with fake-backed tests; the joint gateway proof (native agent to MCP through the gateway,
blocked connections) passed on a Cilium cluster and needs a NetworkPolicy-enforcing CNI to repeat.

## Initial installation and history boundary

New main and child sessions retain their coordination grants, and new owner workspaces enroll
only through the normal workspace creation path. Migration preserves active main/child hashes
and references but leaves existing `agent` workspaces ungranted; observed standalone sessions
still receive no binding or credential. An unresolved create keeps its original grant and
credential-reference contract. Existing native history is not rewritten or transferred.

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

## Native structured input

Pending native HITL requests are discovered without an open chat through the background
observer. Chat and inbox consumers use the same request reference and dedicated owner
response route; a normal chat message is not a structured approval. See
[HITL discovery and continuation](merge-policy.md#background-discovery-and-structured-continuation).
The main thread and session chats show the same structured-input card as the inbox. Cards
refresh while mounted and visible, with a nominal four-second interval. Tool batches require an explicit approve/reject
choice for every call; rejections may include a reason. Questions show single/multiple choices
or free text when choices are empty/null. Answers use the dedicated HITL route, never the
ordinary chat composer or generic queue response route.

A recorded answer disables further decisions. Delivery uncertainty, stale observations,
unavailable continuations and server-disabled writes are visible separately from task activity.
Refreshing reads status; it never replays a decision. An unconfirmed browser submission remains
locked until a receipt is observed. A newer request gets fresh controls keyed to its own ID.
Claude rejection reasons are included in the denial message; Codex reasons are saved but its
runtime receives rejection only. An unknown leaf provider makes no reason-delivery claim.
Valid questions render for any provider, without claiming native Claude Ask User support.

Verified aliases link to the selected continuation card. Bound requests link to the original
conversation; observed standalone requests explicitly state when no Mainloop conversation
exists. Agent-supplied request data is escaped text, and unknown fields remain inspectable.
See [inbox](inbox.md) and [project policy](projects.md).
