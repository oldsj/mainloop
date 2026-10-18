# Chat

The home chat is the user's native Claude Code main session, run by a kagent Agent. The provider owns native session history and tools; Mainloop records logical messages and delivery state.

## Go server status (G6a, G6b)

Status: **implemented** in `server/internal/sessions`, proven by tests against a scratch PostgreSQL and the fake kagent (`kagenttest`). **Not measured** against a live kagent.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

- `POST /chat` records the user message and a delivery row in one transaction and answers `200` with `status: "ok"`, `pending: true`, `message: null`, `spawned_session_id: null` and `delivery_message_id`. It makes no kagent call; a reconcile loop delivers the row. `conversation_id`, when sent, must be the main session's (`404` otherwise). An empty or over-long message is `422`. While a turn is open (`recorded`, `sending` or `delivered`) or an `uncertain` delivery is within its hold (30 minutes by default, `SESSION_UNCERTAIN_HOLD_SECONDS`), a second message is recorded durably as `queued` instead of refused, and the answer is `202` with `status: "queued"` in place of `200`; everything else about the response is the same. The open-turn rule is a partial unique index on `deliveries(session_id)` that deliberately excludes `queued`, so any number of messages may wait their turn there, admitted one at a time, in order, as each open turn ends — see [sessions](sessions.md#go-server-status-g6a-g6b).
- `POST /chat` has no message id in the request, so the API gives each call a new id. The service call `Send(messageID, …)` is idempotent per id: repeating an id records nothing new.
- The reply is the task's artifact text, else the last agent message of its history. It is mirrored into `session_messages` with an id derived from the task id, in the same transaction that settles the delivery as `completed`. A task that failed, was rejected or was stopped mirrors only the artifact text it had produced, if any; its reason is the delivery's detail.
- Every transaction that settles a delivery (`completed`, `failed`, `cancelled`, including "not sent again" after the uncertain hold) publishes `session:message` last in that transaction, so the page re-reads the session whichever way the turn ended. `message_id` is the mirrored reply, or the user message when nothing was mirrored.
- `GET /main-thread` lists the newest 20 deliveries, oldest first, `queued` ones included. `turn_in_flight` is true only while a delivery blocks a send, by the rule `POST /chat` applies: an open one, or an uncertain one inside the hold (a message recorded as `queued` behind either is not itself blocking, and never shows as in flight). Inside the hold the note says new messages wait; once the hold has passed and the loop has not yet settled the row, it says the message was not sent again — looking past any `queued` message already waiting behind it, since that one has not been attempted yet and says nothing about whether the uncertain one was.
- `GET /main-thread` and `GET /conversations/{id}` only read. Before the reconciler has created the main session (it runs a sweep every `SESSION_ENSURE_SECONDS`), `GET /main-thread` answers with `session_id`, `conversation_id` and `native` null. The conversation has no summary (`summary` and `summarized_through_id` are null). `session_state` is read from kagent best effort; if kagent is down the note says so.
- Stop turn (G6b) is implemented for the main thread and every other session: see [sessions](sessions.md). The report queue (`task_event_deliveries`) is a separate ledger from the sessions delivery ledger's own `queued` state (see below); both now admit into a busy session instead of being refused.
- A delivery that stays `uncertain` while its hold passes and kagent lookups keep failing publishes `session:message` once when the hold passes, so the page shows the "not sent again" note without waiting for a lookup to succeed.
- While a turn runs, the page shows it live (implemented; web tests run against MSW, not a live kagent). `session:progress` events (see [events](events.md#live-progress)) carry the partial reply, a status line (`Thinking…`, `Running Bash: go test ./...`, `Reading file.go`, `Waiting for you`) and compact tool lines. The page renders them as a dimmed bubble below the last message, with a timer counting from the turn's start. Stop is unchanged. The bubble is polite to screen readers: it announces the status line only, not the growing text. Live state is memory only: the mirrored reply is still written once, at settle, and `session:message` still triggers the re-read. The closing frame repeats the last text and tool lines. When it names the stored reply, the bubble keeps showing them until the page has read that message and then disappears, so there is no gap and no duplicate. A page opened or reconnected mid-turn is brought up to date by the snapshot the stream sends after `connected`. If kagent's stream drops, the last live state stays up, with its timer still counting, while the thread is busy; the delivery then settles as before and the reply replaces it. With nothing to show (a harness that sends no text yet) the bubble shows the status and elapsed time only.

## Selecting the main provider

Status: **implemented** in `server/internal/sessions` and covered by scratch-PostgreSQL tests against the fake kagent. **Not measured** against a live kagent. The owner's Main provider select is built in the main-thread header (below). Its React tests run against MSW, not a live kagent. The routes below are the contract it uses.

`MAIN_PROVIDER=claude|codex` selects the first-use default (`claude` when unset). An existing main binding keeps its persisted selection across restarts and configuration changes. The owner reads `GET /main-thread/provider` and selects with `PUT /main-thread/provider` and `{"provider":"codex"}` (or `"claude"`). These routes sit behind the owner API boundary and the cross-origin write guard (`403` for a foreign origin). There is no MCP tool, so agents cannot select the main provider, and there is no quota-driven fallback.

The response exposes `desired_provider`, `effective_provider`, `status`, the current `kagent_session_id`, a bounded `error`, `recoverable`, and the retained native identity `history`. `/main-thread` includes the same state as `provider`. `not_started` means the binding has not created a native Session yet. `pending` means a switch contract is committed but preparation has not completed, and `failed` means its latest preparation failed; both have a null effective provider. A pending target may have a native Session id before readiness is confirmed. A switch whose preparation did not finish is still a `200` with `pending` or `failed`; the request is not the retry loop.

A switch needs an idle main: no recorded, sending, delivered, uncertain or queued delivery, no unconfirmed stop, no open native task and no unsettled native lifecycle operation. Failing to establish idleness returns `409` and changes nothing. Before anything changes, the target Agent is read from kagent: a missing or unprepared Agent (no ready revision) returns `409` `agent_unavailable` and changes nothing, restart included. The old native Session is suspended, kept in the audit history, and has its MCP credential revoked. A main that never started has no old Session, so a switch of it writes no history entry. The new provider gets a new native identity and credential, with its own Secret named per identity, so revoking and cleaning up the old one never touches the new one and the create does not wait for it. Its create request, Agent reference and idempotency key are committed before any kagent call, and no transaction or lock is held across a kagent call. The product session, conversation, messages, topics, child task identities, parent references and capacity stay as they are. The new Session is not started blind: its first message carries a short, labelled preamble with the last `MAIN_CARRY_OVER_MESSAGES` (default 6, at most 100, `0` carries none) user and assistant messages of the conversation, oldest first with their roles, each cut to 600 characters. Only messages of completed deliveries are carried; failed, cancelled and uncertain ones, and any partial reply they left, are left out. The preamble opens with a context block: one line naming the main agent's role, then the shared agent guidance ([delegation](delegation.md#shared-agent-guidance)), which is sent even when there are no earlier messages to carry. It does not render the main's standing context yet (see the delegation spec), so after that block it holds only these messages. The rendered text is frozen on the delivery (`sent_text`) in the transaction that marks it sending, so a not-sent retry sends the same text under the same message id. The binding records the guidance version it has given the Session (and the delivery that gave it). A Session that has its standing context but was given an older version, or none (every main that predates the guidance), gets the context block alone in front of its next message: no earlier messages are carried, because the Session already has them. It is given once, a later version bump gives it again, and the full preamble above records the current version too. If the message ends without kagent having seen it, the next message carries the preamble again, or the context block again when that was all it carried. That covers `not sent`, a refusal, and an uncertain delivery that fails after the hold because no kagent task shows the message. An uncertain delivery keeps the preamble while it is still being looked up. The owner's conversation keeps only the text they wrote, never the preamble, and no earlier delivery is replayed.

A fresh main is created on the configured first-use provider's Agent and follows that setting. Its Agent is pinned on the binding when the first create is requested, so a later configuration change does not move a first-use main either. The pin is dropped again when kagent itself refuses that first create before reserving anything (for example the configured Agent name has a typo), so correcting the configuration takes effect on the next message; once a Session exists the pin stays. A switch pins the target's Agent on the binding, so a later change to the catalog or to `KAGENT_MAIN_CODEX_AGENT` does not move a pending or finished selection. Both main Agents must have the matching native harness and ModelConfig, MCP credential routing and `sessionIdleTTL: 0s`. Claude uses `KAGENT_MAIN_AGENT`, Codex uses `KAGENT_MAIN_CODEX_AGENT` (`mainloop-main-codex`), and one Agent cannot serve both providers. Explicit provider profiles may override the main Agent references. The Kind manifests install both.

Repeating the current selection is idempotent. Repeating a pending selection recovers the same create contract and sends no prompt. After a failed request or restart, read the status and repeat the same PUT. Another provider cannot replace an unresolved selection (`409`). Nothing retries a switch in the background.

If a repeat finds the pending target absent, or settled failed or deleted, in kagent, it records `recoverable=true` durably. The owner can then select the other provider, or repeat the same provider with `{"provider":"codex","restart_failed":true}` to admit a fresh native identity and credential; the abandoned target stays in the history. A lost create reply or an unsettled failure keeps the original contract even when `restart_failed=true` is sent. A create that kagent refuses before it reserves the request id (an Agent without a ready revision, or invalid inputs) holds nothing, so the target is `failed` with `recoverable=true`; a repeat of the same provider recovers the same contract once the Agent is back. A create that landed with its reply lost is bound by the repeat to the Session that exists.

Every refused switch, and a chat message or a `POST /sessions/{id}/message` to the main refused because a switch is pending, is a `409` whose body has a `detail` message and a stable `code`: `busy` (open work, or the main changed while the request ran), `pending` (a switch is pending and must settle or become recoverable first), `unconfirmed` (kagent could not confirm the main idle, the old Session suspended or the pending target absent), `unresolved` (a native create for the main is unresolved) or `agent_unavailable`. Other conflicts carry no `code`. Only a status that kagent sent counts as a refusal before reservation; a reply Mainloop could not decode (cut short, wrong shape) leaves the create unconfirmed, because it may have committed, and a repeat binds the Session that landed.

The owner's Main provider select sits in the main-thread header's top strip, always visible and one tap away: it is never hidden behind "details ▾". It sends one explicit `PUT` per choice and shows the confirmed provider; choosing the ready provider again sends nothing. The select and Restart are held while the main is busy: offline, a send or stop in flight, the thread or conversation still loading, a switch pending, or a turn or delivery open. Retry is held by the same conditions except a pending switch, so a pending switch offers Retry; one click repeats the same `PUT` (the desired provider, `restart_failed: false`) and no other write. While a switch is pending, the page re-reads the provider state with `GET` only, on the poll interval, until it settles. Drafts and child session composers are not affected. The rest of the provider row (the confirmed provider, status words, Retry/Restart and any notice) shows inline next to the top strip while it needs attention (failed, switching, unconfirmed, offline, or a notice from the last selection), and otherwise stays under "details ▾".

The main composer is locked while the provider is selecting, its state is unconfirmed or unknown, or the provider is failed, preparing (pending), or ready but not yet the desired one; it says why. A `not_started` main takes the first message. "Working…" shows only while a turn is in flight. An open delivery that is not a turn holds the select and Retry but does not lock the composer. A refused switch, and a chat message refused for the same reasons (its notice starts "Message not sent"), shows one notice whose wording follows the `code`: `busy`, `pending`, `unconfirmed`, `unresolved`, or `agent_unavailable` (the other provider is not installed here, and nothing changed). An unknown or missing code shows the server's `detail` instead. A chat message refused with any other 4xx shows its `detail` the same way, followed by "Your message is back in the box."; a 5xx shows only the generic line. Every notice says the state is re-read and nothing is retried on its own. The API client keeps the `code` on these errors (`ApiError` for the select, `SendError` for a chat message), so the notices come from the live response. A `code` the contract does not name is dropped and the `detail` shows.

While a switch is pending, `POST /chat` returns `409`. The orphan sweep refuses to run while a main create is pending and says so in its report, with the repeat that settles it. A child report to the main waits in the report queue and is delivered by the normal delivery reconciliation once the target is ready.

Bootstrap reuses a main only while its binding has active coordination authority. Otherwise it creates the replacement and releases the old owner slot in one transaction; concurrent requests share one replacement, on the first-use provider, and the old binding's credential is revoked. Earlier main rows are not backfilled or migrated.

## The `/model` command

Status: **implemented** in `web/src/features/chat`, client-side only; covered by component tests, not run against a live backend.

Mainloop has no model registry and no reasoning-effort field anywhere in its contract or in kagent's session API: the LLM a provider's native session runs is fixed by kagent's Agent/AgentTemplate/ModelConfig chain, set by operators outside this repo, which Mainloop neither stores nor injects (see [credentials](credentials.md)). The main provider (Claude Code or Codex) is the only axis Mainloop can actually switch at runtime, so `/model` is a composer-native way to drive the existing main-provider switch above, not a new model or effort mechanism.

Typing `/model` in the main composer and sending it is intercepted client-side: the text is never posted as a chat message, and the sent draft is cleared the same way an accepted message's is. `/model` alone shows the current provider, the status (`ready`/`not_started`/`pending`/`failed`), the available provider names, and a fixed line stating that reasoning effort is not supported and why. `/model <name>` (for example `/model codex`) calls the same `PUT /main-thread/provider` write the header's provider select uses, subject to the same idle, busy and conflict rules, and the header shows the switch's progress and any refusal as it always does; a name that is already the ready provider reports a no-op and writes nothing, and a name the registry does not have is reported as unknown, not switched. A second argument (a reasoning effort) is refused outright with the same fixed line, rather than silently accepted and discarded. The command's own output is a transient status line above the composer; it is not a conversation message, is not persisted, and is replaced or cleared by the next command or ordinary send. There is no command for a session's own chat, only the main thread's.

## Sending messages

- The input field submits one user message to the main session.
- Mainloop records the conversation message and delivery intent before contacting kagent. The message id is the A2A `messageId`.
- A prompt is sent once. If kagent reports it did not accept the message, Mainloop retries the same message for up to 30 seconds, then marks the delivery failed (not sent); a failed delivery is not requeued. If the outcome is otherwise unknown, Mainloop looks for the task by message id; if none shows it, the delivery is marked uncertain and is never replayed.
- The response is the completed A2A task's text, mirrored into the conversation once. While the page is open it polls for the mirrored reply.
- A message sent while a delivery is in flight is recorded as `queued` instead of rejected (`202`), and delivered in its own turn, in order, once the open one ends.
- Each user message carries its `source` (`GET /conversations/{id}` and the session conversation): `user` for the owner's own, `mainloop` for one Mainloop wrote on the owner's behalf, namely the delegated brief and the task reports sent to a supervisor or the main session. Assistant messages have none. The web shows a `mainloop` message as a collapsed "Assignment from Mainloop" (a report keeps its own label, the task's kind, short id and outcome, and is shown open so an `Owner action needed:` line is never hidden) and never as "You". It reads the source from the message, so it holds however far the thread has gone; the delivery ledger shows only the newest 20 deliveries and is not used for this.
- Each assistant message likewise carries the `provider` (`claude` or `codex`) of the native agent that actually produced it, resolved from the delivery that wrote it rather than the session's current binding, so a reply keeps its own label after the main thread switches providers — it is absent when the writer cannot be determined.
- A failed delivery keeps a short reason, stored with the delivery: the kagent/A2A error class and message (for example `not sent: SessionError: …`, `send rejected: A2AError (INVALID_PARAMS): …`, or `task failed: <kagent's message>`). Best-effort redaction covers credential assignments, authorization headers, bearer tokens, URL userinfo and long opaque tokens; arbitrary harness text is not guaranteed to be secret-free. Whitespace is collapsed, and the text is cut to 300 characters.
- The reason is shown on the message it belongs to, in a `Failed`/`Unconfirmed` notice. The header and identity strip show `last message failed` (red) or `last message unconfirmed` (yellow) instead of `ready` or `working`; a delivery still in flight takes precedence and shows `working`. The notice survives a reload because it is read from the stored delivery.
- A failed (not uncertain) owner (`user`) message offers **Retry** while it is the newest delivery and nothing is in flight. Retry checks fresh delivery state and always targets the owning conversation, even when a child session is selected. A local send guard stays held until submission and refresh finish, so polling cannot enable a second submission. Retry sends the same text as a new message with a new message id; the failed message and its id are never re-sent. An uncertain delivery is not retried, because the original may have been accepted. A failed brief or report offers no Retry: the task loop re-sends those itself.

## Conversation history

- User and assistant messages persist across page reloads.
- The native session remains authoritative for provider history and context management; Mainloop mirrors observed messages and delivery receipts.
- The main session keeps one kagent Session until kagent deletes it (for example after its idle TTL); then the next message starts a new one, see Sessions. Context length is managed by the provider's native auto-compaction, which is configured per harness outside Mainloop; Mainloop does not rotate the session or ask it to write out state. The first message to each kagent Session, the first ever, one after a provider switch, or one after kagent deleted the Session, carries the context block and the recent completed conversation (see the provider section); later messages go as written.

## Delegating tasks

- Main creates durable supervisor tasks through `delegate`; each supervisor may create direct children in its inherited project/tree. Children cannot delegate further.
- Explicit progress and result reports persist against their task attempt and queue notifications to the current authorized parent and main. Busy parents use the existing delivery ledger.
- `task_get`, `task_list` and `task_history` inspect stored progress without native turns. Task cancellation uses the shared service; retry/reassign remain unavailable until handoff is installed.
- Native turn completion creates no automatic report or task success. Coding success requires verified merged publication; coordination completion requires explicit success, no live children and confirmed runtime settlement.
- See [delegation](delegation.md) for inputs, scope and restart behavior.

## Identity and policy

The identity strip shows the native agent, the kagent Agent and Session with its runtime state, model, turn count, and delivery states. Tool identity comes from a per-binding credential reference injected by kagent's credential path; the native agent receives a placeholder, not the bearer. MCP discovery and calls use the binding role and persisted grant together. The main thread manages topics, pending intent and its durable task trees. Task supervisors and children can report their own current attempt and read scoped task history; children cannot delegate or inspect siblings. A newly created owner workspace can call `whoami` and `open_pull_request` within its stored project and branch scope. Merge tools remain behind the existing enablement gates. Ordinary owner workspace grants do not provide topic writes, reports or delegation. Delegated workspace grants additionally supply task tools within their persisted task scope; coordination grants never supply repository authority. Ordinary standalone and retained legacy workspace bindings stay ungranted. Inputs are validated and policy failures return tool errors. Terminal, archived and revoked bindings cannot authenticate. An MCP tool write re-reads the caller's credential `FOR SHARE` inside its transaction and requires it to be published, so a revoke waits for an in-flight write and a write after the revoke is refused (`unauthorized`). Every write tool in the contract writes inside such a transaction (a test calls each one with a revoked credential and checks that no table changed, and fails for a tool that is not classified as a write or read-only). A tool's reads are not guarded, and a revoke that waits on a tool transaction holding other locks can deadlock with it, in which case Postgres aborts one side.
The dedicated MCP origin exposes `/mcp` and the isolated `/mcp/merge-approval` tool surface; merge tools remain disabled pending the [merge enablement gates](pull-requests.md#enablement-remains-separate). REST remains unauthenticated and relies on the
required NetworkPolicy isolation documented in the architecture guide. The channel is
implemented with fake-backed tests; the joint gateway proof (native agent to MCP through the gateway,
blocked connections) passed on a Cilium cluster and needs a NetworkPolicy-enforcing CNI to repeat.

## Initial installation and history boundary

New main and child sessions retain their coordination grants, and new owner workspaces enroll
only through the normal workspace creation path. Existing `agent` workspaces stay ungranted, and
observed standalone sessions still receive no binding or credential. An unresolved create keeps its original grant and
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

## Go/React status

Implemented in `web/src/features/chat` against the contract (`GET /main-thread`,
`GET /conversations/{id}`, `POST /chat`). It has not run against the Go server yet; tests use
MSW fixtures typed from `api/openapi.yaml`.

- The main thread is the only chat surface. There is no session picker, no thread replies, no
  zoom, and no fallback to an older conversation list.
- A send shows the message at once. If the backend refuses it, the bubble goes away and the
  text returns to the box; once it is recorded, a later failure to refresh does not undo it. The
  bubble stays until the conversation lists the recorded message, matched on the response's
  `delivery_message_id` (the native backend returns no message body), or its `message.id`.
- A send that gets no reply (no response) is checked before its text goes back. The deliveries are
  read again just before the send, so a delivery recorded earlier is not counted. After the lost
  reply the thread is read again. Only a new user delivery counts: reports and other sources do not.
  A new user delivery is this message only when the conversation holds a user message under that
  delivery's id whose text matches the sent text (both trimmed). Then the notice says the message
  was recorded, the box clears, and the message shows in the conversation. If no new user delivery
  landed, one more read after a short delay checks for a delivery still being recorded; the send
  stays locked and no message is resent. If that also finds none, the text returns to the box.
  If a read fails, or a new user delivery's conversation cannot be read or holds no
  matching message, the text also returns to the box, and the notice says Mainloop could not confirm
  whether the message was recorded. The text match cannot tell this message from another client's
  message with the same text sent in the same moment, so that case can read as recorded.
- The box is locked until the thread has loaded, so early typing cannot land in the wrong draft.
- Drafts are kept per conversation in `sessionStorage`. Only a recorded composer send clears
  its draft; Retry leaves whatever is in the box alone.
- **Implemented:** a suggestion chip, or **Ask Mainloop** on an explorer row (its button, or the row menu from a right-click, a long press or Shift+F10), opens the main chat with the composer prefilled, for example `Start a task in <project full name>: `. There is no **New task** button. The text goes in the box once, unsent, with the caret at its end; if the box already holds a draft, the prefill goes before it. The prefill rides in the `prefill` search parameter and is removed from the URL once the composer has taken it, so a refresh does not prefill again. No task is created until the owner sends the message.
- A conversation that fails to load shows a recovery message, even when it has no messages yet.
- While a turn is in flight the thread is re-read every two seconds, for six minutes from when
  the turn was first seen in flight, as a fallback to the `session:message` event. The
  conversation is read once more when the turn ends. Reads pause while the tab is hidden.
- Pending requests of the main session and `?hitl=<id>` render through the `features/hitl`
  approval card, inside an error boundary so a failing card cannot take the thread down. They sit
  at the end of the conversation, after the newest message, and scroll with it: the message list
  is the only scroll in the centre column. The cards sit after the conversation log, not in it, so a
  screen reader does not announce their every change. While the owner is at the bottom, new messages
  keep the view there, and a card that fills in is brought into view from its top. Once the owner
  works inside a card (**Show all**, **Request data**), its growth moves nothing until the next
  message.
- An empty thread shows suggestion chips inside its empty state (**What needs me?**,
  **Start a task in <first project>**, **What's running?**, **Pause a schedule**). Each fills the
  composer, unsent. They go once the thread has a message.
- The native identity strip in the header details is supplied by the sessions slice.
