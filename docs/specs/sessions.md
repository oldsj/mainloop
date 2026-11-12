# Sessions

Sessions are native Claude Code or Codex work started from the home thread or the `/agents` page. Mainloop keeps the product session, workspace lifecycle, native binding, and message delivery as separate records.

## Go server status (G6a, G6b)

Status: **implemented** for the main session (G6a) and the session routes (G6b) in `server/internal/sessions`; tests use a scratch PostgreSQL and the fake kagent. **Not measured** against a live kagent.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

- The main session is created by the reconciler (a periodic sweep) and by the first `POST /chat`, never by a GET. Concurrent first calls share one session.
- Delivery states in the Go ledger: `queued`, `recorded`, `sending`, `delivered`, `completed`, `failed`, `cancelled`, `uncertain`. Only `recorded` is ever sent: the claimer commits `sending` before the kagent call, holds no database lock or transaction across it, and fences every outcome write on its lease and attempt count, so a claimer that lost its lease changes nothing. `queued` is a message written while another turn was already open; the partial unique index that enforces one open turn per session (`recorded`, `sending`, `delivered`) excludes it, so any number of messages may wait their turn, each in the order it arrived. Whenever a turn ends — settled by the delivery loop, or stopped by the owner — the oldest message still `queued` for that session, if any, is admitted (moved to `recorded`) in the same transaction, one at a time. A session cancelled outright (not just one turn stopped) cancels every message still `queued` behind it instead: there is no next turn for them to wait for. This `queued` is the sessions ledger's own state, distinct from the task report queue's `queued` (`task_event_deliveries.state`, see [tasks](tasks.md#implemented-reports-to-parents-task-event-deliveries-loop)), which now simply admits at once instead of polling `ErrBusy`.
- kagent is retried only when it says the message was not accepted, with the same message id and inside the send retry budget (the kagent client does this). Every other doubt makes the delivery `uncertain`, and it is settled by looking the message up (`GetTask`, else `ListTasks`), never by sending again. A `sending` row reclaimed after a crash goes straight to `uncertain` and is looked up. If no task shows the message once the hold has passed, the delivery fails with "not sent again" and the owner may send a new message.
- Before a send, the loop makes the binding's kagent Session usable: it creates it on first use (idempotent by request id) and waits for one that is starting, or that kagent is settling, to be ready (up to `SESSION_READY_WAIT_SECONDS`, 60 by default). It resumes nothing: kagent puts an idle Session's Actor to sleep by itself, a sleeping Session is still `READY`, and the send wakes it. When kagent no longer has the Session (its idle TTL, or deleted out of band) the binding gets a new create request id in a write fenced on the delivery's claim, and a new Session is created (the binding's standing hash is cleared, so the new Session's first message carries the recent conversation again, see the chat spec). A task parked for input or auth is read again at growing intervals up to `SESSION_PARKED_MAX_SECONDS` (300); a failed read backs off the same way. A delivery whose binding is gone fails. The binding records the create request id and the Agent before every create call (`agent_bindings.kagent_create_request_id`, `kagent_create_agent_namespace`/`_name`). They stay set once the Session id is stored and are cleared only when a create is settled or the Session is replaced, so every reader (the orphan sweep, the resolve sweep) also requires `kagent_session_id IS NULL`. A `resolve-session-creates` sweep (every 5 minutes) settles such a binding whose session has no open delivery: it first rotates the binding's `kagent_request_id` (one UPDATE, fenced on the binding still being unbound, the same create request id and no open delivery), so a message that arrives mid-sweep creates its own Session under the new id and the sweep only deletes the one its replay returns. It then replays the create under the old id with the Agent stored on the binding and the same credential reference the create sent. kagent answering that the id belongs to a deleted Session settles it; `AlreadyExists` (a contract mismatch) also settles it, with a warning, leaving the Session to the orphan sweep; a Session that comes back is deleted (confirmed `DELETED`) first. A later send then finds the id deleted and replaces it as above. A delivery stuck because its Agent cannot be resolved logs at error level once past the uncertain hold; one waiting for a workspace that never gets a Session fails after that hold. A workspace session's send is still admitted through the workspace service (`Wake`), which now only refuses a workspace that is being deleted and records the activity. A native or main session follows the configured Agent: a create under a new request id uses it, and so does a replacement Session. A configuration change between a lost create reply and the next send does not fail the message: the send replays the create under the old request id with the new Agent, kagent answers `AlreadyExists`, and the turn treats that like a deleted request id and replaces it, so the message completes on a new Session on the renamed Agent. The Session that landed on the old Agent is then an orphan: every Agent a create was ever sent to is kept (`kagent_agents`, written with the marker and with a workspace), so the orphan sweep deletes it.
- A task-owned session may start with its `/model` and `/effort` override already set (#318): `TaskCreate` and the `delegate` MCP tool take optional `model` and `effort`, validated against the resolved provider profile's `models`/`effort_levels` at task creation (`422` with the allowed values otherwise; see [delegation](delegation.md)), recorded on the task and its attempt, and applied with `SetModelOverride`/`SetEffortOverride` — the same storage and turn metadata as a direct `/model`/`/effort` write — before the attempt's first brief is sent, never after. Leaving both unset leaves the session's override unset too, unchanged from before #318. The chat composer and the `/model`/`/effort` commands ([chat](chat.md#the-model-and-effort-commands)) read and change the same override on a task-owned session as on any other.
- The main session cannot be archived (a database trigger refuses it). At most one main binding exists in the whole database; the owner is a single user for now, so the index is global.
- A task that finished while no process was watching is finalized from `GetTask` on restart. A task parked for input keeps the turn open.
- Failure reasons are stored with best-effort redaction and a 300 character cut (see [chat](chat.md)).
- Routes (G6b), all owner-only and all on the same ledger: `POST /sessions` (create), `GET /sessions` (the owner's sessions except the main thread, newest first), `GET /sessions/{id}`, `GET /sessions/{id}/conversation`, `GET /sessions/{id}/native`, `POST /sessions/{id}/message`, `POST /sessions/{id}/stop-turn`, `POST /sessions/{id}/restart`, `POST /sessions/{id}/cancel`, `POST /sessions/{id}/archive`. Another owner's session and an unknown id are both `404`. `needs_owner` is the attention visibility rule ([inbox](inbox.md)): true while the session has a visible attention item. The list reads it for every session in one query, the detail and conversation routes read it for the one session, and a session just created reads false; its status stays the session's own (only the inbox snapshot reports `waiting_on_user`).
- `POST /sessions/{id}/message` records the message and its delivery exactly like `POST /chat`, with one open turn per session: while a delivery is `recorded`, `sending` or `delivered`, or an `uncertain` one is inside its hold, the new message is recorded as `queued` instead of refused, and the response is `202` with `status: "queued"` and the message id (`200` with `status: "ok"` when it is admitted at once). It is still `409` for a cancelled or failed session, an archived one, one without a native binding, a workspace whose Session does not exist yet, and a task-owned session whose attempt has requested a stop — none of these ever admit a message, so there is nothing to queue behind. The message id is the idempotency key: the caller may choose it (`message_id`, a UUID; the server picks one when it is absent), and repeating the same id returns the state it is actually in (`ok`, `queued` or `steered`) and writes nothing new.
- Steering (`steer: true` on `POST /sessions/{id}/message`): a message sent while a turn runs joins that turn, as a message typed mid-turn does in Claude Code, instead of waiting for it to end. Mainloop steers only when the session's Agent is listed in `MAINLOOP_STEER_AGENTS`, a delivery is `delivered` with a known task, and the text is not a slash command; otherwise the message is an ordinary one (`ok` or `queued`). A steer is recorded first, under the send lock, as a `delivery_steers` row that names the running turn (the message and its id are recorded the same way as any other), and the response is `202` with `status: "steered"`. The steer loop then sends it to kagent as an A2A message on that turn's task with the same message id and the `kagent.steer` flag; kagent hands it to the running SDK turn, which folds it in at its next tool boundary, and the reply is that turn's. Steer states: `recorded`, `sending`, `uncertain` and `delivered` are open; `completed`, `failed`, `cancelled` and `converted` are final. A delivered steer ends with its turn, except one kagent lists as unconsumed (`kagent.steer.unconsumed` on the ended task). A steer that does not join converts once to an ordinary `queued` delivery under the same id, keeping the time it was recorded, so it runs as the next turn ahead of anything sent after it: when the turn ends before it was sent, when kagent refuses it with a steer reason (`metadata.steer` on `KAGENT_SEND_NOT_ACCEPTED`: the task is not running, the driver cannot steer, or the task waits for input), or when kagent lists it as unconsumed. A turn the owner stopped cancels its unsent and unconsumed steers instead. A steer whose send outcome is unknown is settled by reading the turn's task (its message id in the history means it was taken), never by sending it again. Tested against the fake kagent only; the kagent side is a separate kagent change, and none of this is measured live.
- A task attempt's stop intent permanently closes its session to new messages, regardless of
  activity status (including `completed`). Message recording checks the retained attempt's
  `stop_state` in its transaction. Already-recorded deliveries check it before native preparation;
  the admission transaction then serializes with stop. A committed admission decision before
  stop permits the native send, while a stop committed first refuses admission and cancels the
  delivery without sending. Preparation that overlaps a stop rechecks before later native calls
  and admission. A `delivered` message has been accepted by a native task that may still be
  working. Stop confirmation waits for `sending` and `uncertain` outcomes to resolve and a fresh
  native observation showing every task final, with no delivery outcome change since observation
  began. A changed outcome requires another observation before writer resources are released.
  Completed standalone and adopted scheduled sessions remain resumable; they have no product
  task attempt.
- Stop turn cancels the A2A task with `CancelTask` (the kagent client and `kagenttest` both have it). It takes no lock (cancel is the one that takes the per-session send lock); the one-open-turn index and the fenced writes keep it consistent with a send. A delivery still `recorded` is cancelled without any kagent call. The task is cancelled outside any transaction; the delivery ends `cancelled`, the queue is held, and the partial reply and the stop note go into one message in one transaction (the note's id is derived from the delivery, so the delivery loop and the stop call settle the same turn once). Results: `stopped`, `finished` (the task had already ended, and its real outcome is recorded), `no_open_turn`. A task that is not visible yet or still running after the cancel is `409` and nothing changes; a kagent error is `502`. An `uncertain` delivery counts as open while it is inside its hold; one past the hold is skipped, since it no longer blocks sends and the delivery loop fails it, so it cannot mask a newer running turn. The stop is not repeated blindly.
- Restart (`POST /sessions/{id}/restart`, issue #262) is the way out of a turn that stop cannot confirm, such as a runtime wedged inside kagent that never answers `CancelTask`. The body carries a caller-chosen `request_id`, the idempotency key. Under the session's send lock Mainloop first writes the intent (`session_restarts`, state `requested`, naming the kagent Session to delete), then, with no transaction open, reads the Session and deletes it only if kagent still shows it, with kagent's `DeleteSession(force=true)`, which terminates a running turn instead of being refused, and reads it again when the delete reply still shows it running. kagent answering NotFound, `DELETED` or `DELETING` (a delete it has accepted, which cannot be undone and admits nothing) counts as gone. One transaction under the send lock then makes the binding forget that Session (fenced on its id, so a newer Session is never touched; a new create request id, and the standing context cleared), settles every `sending`, `delivered` or `uncertain` delivery sent to it as `failed` with "the native session was restarted before this turn finished; it was not sent again", admits the oldest `queued` message, and marks the intent `done` with the failed ids. Failed, not uncertain: Mainloop knows the Session is gone, so the message cannot still run, and nothing waits out `SESSION_UNCERTAIN_HOLD_SECONDS`. A `recorded` message has not been sent and is left for the new Session. The next message creates a new kagent Session that carries the recent conversation (the replacement path above); the native context is lost. The response is `restarted` (or `no_native_session` when the binding had none) with `failed_message_ids`. Repeating the same `request_id` returns the recorded outcome and deletes nothing; a different id while a restart is open continues the open one. A delete kagent does not confirm is `502` and keeps the intent `requested`; a repeat, or the `resume-session-restarts` sweep (every 30 seconds, for intents idle a minute), continues it the same way. Only the main thread and standalone native sessions can be restarted: a workspace session (its runtime belongs to the workspace service), a task-owned session (the task's stop path force-deletes a runtime that never answers, see [orchestration health](orchestration-health.md#unconfirmed-stops)), an archived, cancelled or failed session, and a main thread whose provider switch is pending are `409`. `NativeSessionInfo.restartable` says whether it applies. Nothing is restarted automatically and no prompt is replayed. Tested against the fake kagent only; not measured against a live kagent.
- A delivered turn whose task shows no activity past its allowance is marked stalled; it is shown with stop and restart and raised as attention, and the next event clears it. It is an observation, not a delivery state: see [orchestration health](orchestration-health.md#stalled-turns).
- A delivered or uncertain turn whose task kagent no longer has (`GetTask` not found, or no task shows the message) is checked against its kagent Session: when kagent shows the Session gone (NotFound, deleting or deleted), the turn fails at once with "the kagent session no longer exists, so this turn cannot finish; it was not sent again" instead of waiting out the uncertain hold, and the queue moves on. A task that is missing while its Session still exists keeps the uncertain path.
- Cancel closes the session's open deliveries except task-owned `sending` and `uncertain` rows,
  whose claims and unresolved outcomes are retained for reconciliation without replay. In the same
  transaction it also cancels every message still `queued` behind them, since the session is ending
  outright and there is no next turn left to admit them into. It then
  sets the sticky `cancelled` status and an `unknown` agent outcome in one transaction (task
  attempt before deliveries, then session, matching send admission and settlement lock order;
  a session that already finished is left as it is), publishing one `session:message` event per
  closed delivery. After commit it lists native tasks in the binding's current context and all
  contexts recorded by deliveries, and cancels non-final tasks, including scheduled turns with no
  Mainloop delivery. A stored binding Agent reference takes priority over workspace and provider
  configuration for cancel, HITL observation and follow-up delivery. The response carries the new
  `status` and `agent`: `stopped` (a matching cancel reply confirms a cancelled task), `not_running`
  (nothing was sent or all observed tasks have finished), or `unknown` (a listing, cancel or
  in-flight send could not be confirmed). The outcome is stored; a repeated cancel returns it
  without another native cancel, including after an interruption. No database lock or transaction
  spans native calls. The main thread cannot be cancelled (`409`). A cancelled or failed session
  takes no more messages.
- Adopted scheduled sessions remain `unknown` while a reserved or in-flight execution can still dispatch, even if native task lists are empty. A periodic sweep revisits those outcomes with durable backoff, cancels late tasks, and settles only after a fresh execution read shows finality or its own task is positively final, followed by another native listing. It never replays the scheduled send or treats elapsed time as finality; unresolved Mainloop sends also retain uncertainty. See [scheduled runs](scheduled-runs.md#native-session-control-implemented) for bounds and evidence.
- Archive clears one finished session from the list. A live session must be cancelled first (`409`, also when the session stopped being finished before the write), and the main thread is never archived (`409`). For a workspace the archived Session is deleted by the workspace service.
- `POST /sessions` creates a native session with its first prompt as a recorded delivery and answers with the session row; the first delivery creates the kagent Session.
- MCP credentials (G6c): the main thread's binding has grant `coordination`, and its first delivery publishes the binding's MCP credential before the kagent Session is created, sending it as the create's one credential reference ([credentials](credentials.md)). Sessions created by `POST /sessions` keep grant `none` and carry none. If the binding's previous Secret is still being deleted, the delivery waits and retries; those waits do not count against the create budget. Failed creates are counted per delivery in `deliveries.create_failures` (five failures settle it as failed), separately from claims (`attempts`), which also count credential waits and lease recoveries. Cancel revokes the session's credentials in the same transaction as the status change, and archive in the same transaction as the archive. Outside a cluster (no Secret store) the main thread is created without a credential and logs a warning. Cancel and archive end the binding for good: any future un-cancel or un-archive must re-`Issue` the binding's credential (the revoked one is not restored), and its create replays must then carry the new reference.

## Session list

Sessions appear in the explorer, not in a list of their own: current sessions under
**Mainloop**, and a task's session under that task in its project. The explorer is a side bar
on the right at widths of 1024px and above, and the Explorer tab on narrower windows, where the
Chat tab holds the conversation. Task detail scrolls within the available pane; workspace
controls wrap when needed.

The native main thread and archived sessions are not listed as rows; finished sessions show
only under their task. There is no create button: new sessions start from the main chat (the `/agents` form is reachable by URL but not
linked).

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
- Each native session maps to one kagent Session (created on first use; kagent puts an idle one to sleep and the next send wakes it) on the configured kagent Agent for its kind. Mainloop does not create a Claude SDK worker for each session.
- The ordinary `/agents` creation path creates an ungranted `agent` binding. The separate
  validated `POST /workspaces` path enrolls a new owner workspace with a per-session MCP grant
  and stored project/repository/branch scope. Existing workspace bindings are not enrolled by
  migration; see [workspaces](workspaces.md) and [agent credentials](credentials.md).
- If kagent has deleted that Session (its idle TTL, or out of band), the next message creates a new one under a fresh request id and carries the recent conversation again in its first message (see the chat spec). The provider's earlier context is gone; Mainloop's conversation history is kept. Turns still open on the deleted Session fail at once, not sent again (see above). A `failed` kagent Session is reported, not replaced.
- Each user message is recorded with a delivery state before it is sent. Delivery states include `queued`, `recorded`, `sending`, `delivered`, `completed`, `failed`, `cancelled`, and `uncertain`. kagent allows one non-quiescent task per Session, so Mainloop queues messages itself: both the owner's own messages to a busy session (`queued`, admitted in order as each turn ends) and task reports to a busy parent or main thread (the separate report queue, `task_event_deliveries`). A task waiting for input (`input-required`) stays `delivered` and blocks further turns until it is answered or the turn is stopped; the shared HITL card can answer it when the server permits owner responses.
- A message still `recorded` after a backend restart was never sent, so it is delivered then; this is its first send, not a replay. A `sending` message with no task after 60 seconds, including when the lookup itself keeps failing, becomes `uncertain`.
- A failed or uncertain delivery stores a short reason with best-effort redaction (see [chat](chat.md)). The session chat shows it next to the message and the identity strip shows the last problem, and the same Retry rule applies: a new message with a new id, offered only for the newest failed user message with nothing in flight.
- An uncertain delivery is never replayed automatically. A message sent while another turn is in flight is recorded as `queued` rather than rejected (see above); it is admitted, not resent, once that turn ends. A message for a suspended workspace resumes it first; one that arrives while the workspace is being suspended waits for the suspend to finish, then resumes it.
- Session conversations mirror the user's messages and each completed task's reply. The reply id is derived from the task id, so a repeated observation mirrors it once. The transaction that ends a delivery also stores the health observations of the task it holds ([orchestration health](orchestration-health.md)); if that fails, the delivery still ends and the health sweep collects it later.

## Cancelling and clearing

- Cancel ends the session and cancels its open A2A tasks. If Mainloop cannot confirm the stop, it reports that result and does not repeat the stop blindly.
- A cancelled session no longer accepts messages.
- Stop turn (`POST /sessions/{id}/stop-turn`, owner only) cancels the open A2A task with CancelTask and keeps the session and its kagent Session, so a parked (`input-required`, `auth-required`) or runaway turn can be cleared. It works for the main thread and for every other native session. The main thread has a `stop` button next to its working indicator; every session detail page offers **Stop turn** independently of Cancel session, but only while the agent reports a turn in flight. Archived, failed and cancelled sessions hide it, and so does a native state the page cannot read.
  - The delivery ends as `cancelled`, the queue is held, and the conversation gets the partial reply followed by "This turn was stopped before it finished.", in one transaction. Streamed text is retained durably even if the cancellation response omits artifacts. The reply is visibly marked as stopped. Cancellation is recorded only after kagent confirms it (or before sending a still-recorded message). The same transaction admits the oldest message still `queued` for the session (if any): stopping one turn starts the next one waiting behind it, not just reports.
  - Queued reports stay queued across refreshes and restarts. The owner's next explicit message releases the hold and starts a new task on the same Session, ahead of the held reports. The reports follow once that turn finishes. A report alone never releases the hold.
  - Stopping with no open turn changes nothing (`no_open_turn`). If the turn completed or failed first, that outcome is recorded and the response is `finished`.
  - A kagent error returns `502` and leaves the delivery as it was; a send with no visible task yet, or a task that is still running after the cancel, returns `409`. Neither is retried automatically.
  - A task observed as cancelled from any other source (a stream, a sync) also ends the delivery as `cancelled`, with the same partial text, note and queue hold.
  - When a stop fails (kagent error or the task still running after the cancel), the chat offers **restart session** for a restartable session, behind an in-app confirmation that says the native context is lost. It calls `POST /sessions/{id}/restart` (above), reusing one request id until it succeeds.
- The archive API retains finished sessions for audit. Live sessions must be cancelled first.
  The list's history toggle does not archive or delete sessions.
- Task cancellation uses the durable task controls; native session cancellation remains available
  where the current attempt's authority permits it.

## Session detail

Assistant message prompts and processing indicators use the native runtime kind (`claude` or `codex`), including the main thread, inline replies, and fullscreen session views. Unknown or missing identity is labelled `agent` rather than guessed from a model name.

The session view shows the conversation, session status, and a native identity strip with the agent kind, model, kagent Agent and Session state, turn count, delivery states, and the reason for the last failed or unconfirmed delivery. Workspace health and lifecycle controls are shown separately.

The owner-only `GET /sessions/{id}/native` (and the main thread's `native`) also returns an optional
`preparation_observation`, projected from the kagent GetSession response it already reads for the
Session state: one GetSession, no second RPC. It includes Session/context identity and whether the
context was explicit, state/operation and prepared revision, the observed runtime generation/Actor UID,
and allowlisted original preparation action/create/generation identity, setup profile/digest,
classification, historical flag, challenge/sequence and effect/observation timestamps. Timestamps
retain protobuf seconds/nanoseconds. Absent optional messages and empty identity fields stay
unavailable; proto3 scalar defaults are retained without claiming field presence. `receipt_status`
distinguishes `observed`, `missing` and `unavailable` (a receipt that cannot be safely projected,
including a foreign identity or a value outside the contract's bounds). Runtime association and
receipt are independently optional. The observation itself is absent, and with it `session_state`
(the note says "kagent session unavailable"), when GetSession is unavailable or its reply is for
another Session or context, repeats the workspace preparation or the runtime association on the wire,
carries a receipt that names no original request, or has runtime fields that cannot be projected
(an unknown state or operation, an identity outside its bounds). The duplicate check runs on the raw
reply before protobuf merges the copies, in the kagent client's session codec, and also refuses a
repeated singular field inside the receipt; the call then has an unknown outcome. A reply that carries the Session or the receipt twice at its top level (for example a
split `GetSessionResponse.session`) is refused the same way, while the repeated `sessions` of a list
reply stay allowed. A Session that states no context id is shown with its own id as the context and
`context_confirmed: false`, read through a display-only client call (`ObserveSession`); delivery,
lifecycle and readiness keep requiring a confirmed context, and a context naming another Session is
refused either way.
The response excludes credentials, configuration documents, checkout content and arbitrary runtime
data: only the fields in `NativePreparationObservation` of `api/openapi.yaml` are ever copied. This
diagnostic API does not add a UI flow or a second RPC, refresh preparation, start work, or grant
dispatch authority. Runtime `ready` alone does not prove preparation; saved receipt evidence does
not identify an earlier runtime admission failure's cause. The `web/` client has the generated types
only; no screen shows the observation yet.

### Live sessions (SDK runner)

When `NativeSessionInfo.live` is true (the session's Agent runs the SDK runner driver), the session
page replaces the identity strip, actions and chat with a live view attached to
`GET /sessions/{id}/live` (WebSocket, subprotocol `kagent.sdk-runner.v1`, the runner's Client event
protocol). It renders the way Claude Code does: assistant text after `●`, tool calls with their first
output line after `⎿` (tap to expand), edits as diffs, permission prompts with numbered answers, and
an interrupted turn as `⎿ Interrupted`; a finished turn prints nothing, and there are no status icons
or cost lines. Input goes through Mainloop's API, never the socket, so every message is in
Mainloop's record before it reaches the runner. The composer sends each message with
`POST /sessions/{id}/message`, with a message id chosen in the browser and `steer: true`; it shows at
once and the runner's echo replaces it (matched by the message id the runner echoes as `mid`, or by
text while it does not). A failed send is retried twice under the same id, then marked "Not sent". A
message sent while a turn runs steers it (see Steering above) and shows as "queued · joins this turn"
until the turn takes it; one Mainloop queues instead, such as a slash command, shows as "queued ·
sends when this turn ends". Slash commands go as typed, except `/model` and `/effort`, which are
Claude Code's own: without an argument they open the model and effort picker, and with one (or from
the picker) they set the session's override (`POST /sessions/{id}/model` or `/effort`) for its next
turns and print "Set model to …" without reaching Claude. Up on the composer's first line (or in an empty composer) recalls the previous sent message and
Down on its last line steps forward, back to the unsent draft, which is kept; elsewhere the arrows
move the caret, so multi-line editing works. History is this session's user messages (the attach
backlog and those sent since), a repeat kept once. On a phone, a small `↑ history` button below the
composer lists the last ten to tap into the composer. The send button becomes interrupt while a turn runs and the composer is empty; Esc interrupts
too. Interrupt is Mainloop's stop turn (`POST /sessions/{id}/stop-turn`), which keeps the partial
reply, as Esc does in Claude Code. Shift+Tab or the mode button below the composer cycles the permission mode. The runner
prompts live only for a turn a client started on its port and for prompts outside the harness's
approval boundary (the profile's own permission mode); those are lost if the session sleeps. A
protected tool call or question in a turn Mainloop sends (an A2A turn) is deferred instead
(kagent #90): the live turn ends with a `tool_deferred` result, the log says it is waiting for the
answer, the status reads "needs you", and the request shows at the end of the transcript as the same
card the chat view shows, answered through the A2A task. Until it is answered the composer says to
answer the request first, and a message sent anyway is queued by Mainloop behind the waiting turn;
the answer replays the call, the turn continues, and the queued message runs after it.

The view never wakes a sleeping session on its own (#348): an attach goes through the Substrate
router, which wakes the actor, so a view that reconnected whenever kagent suspended the actor would
keep it awake for as long as the page is open. Before every attach the owner did not ask for, it
reads `GET /sessions/{id}/native`; when the runtime association says the actor is not current, the
view shows "asleep" with a Wake now button and does not attach (an absent association is unknown
and attaches as before). Opening the page of a sleeping session therefore does not wake it. A
message sent from the composer while asleep, Wake now, and Retry now are owner actions: they attach
with `wake=1` (the message is recorded and sent by Mainloop at once, which wakes the session as
well). Asleep, the view reads
the native state every 10 s and attaches without waking once something else woke the session (a
turn Mainloop sends, or an answered request that replays the call). An attached view also keeps the
actor awake by itself: the runner counts every open client as busy (kagent's SDK runner), and kagent
suspends the actor only about 30 s after the last one detaches. So the view detaches on purpose
after 2 minutes with nothing happening (no turn running, no prompt unanswered, nothing received,
sent or typed), or 15 s after the page is hidden, and shows "paused" with a Resume button. A paused
view does not attach again while the actor is still awake: it waits until kagent reports it asleep
and then behaves as asleep, so it attaches by itself only after something else woke the session.
A permission prompt the runner holds live is lost if the session sleeps, so the view does not
detach while one is unanswered, hidden or not (the phone dropping the socket still can). A drop while the session is awake
re-attaches with backoff (at least 1.5 s after an attached socket drops, so a suspend can show first)
and immediately when the page becomes visible again. The transcript is seeded from Mainloop's
conversation (`GET /sessions/{id}/conversation`), which survives sleep: it is shown above the live
entries under "Earlier in this session, from Mainloop's record", and stops before the last copy of
the user message the runner's `hello` backlog starts with, so a turn is not shown twice. Every attach
rebuilds the live part from the backlog, which is in the runner's memory and starts empty after a
sleep or a replayed answer; the record is read again on each `hello` and each sleep. The record holds
the messages and replies of turns Mainloop sent (A2A turns), which since the composer sends through
Mainloop includes every message sent from the live view; tool calls from earlier runner processes
are not shown. After five
failed attaches in a row with no `hello` (about 15 s, longer than a wake's 503s normally last) the
view says the live channel is unavailable, keeps retrying every 8 s, and offers Retry now and Use the
chat view (the regular session view, until the page is reopened).
`?live=1` on a session URL forces the live view for local work; with
`VITE_MSW=1 pnpm dev` a scripted fake runner answers (`web/src/features/live/fakeRunner.ts`, also
used by the tests).

The server relays the WebSocket frame by frame to the runner's client port (`:7682/ws`) on the
session's actor: it ends the browser's WebSocket and dials the runner as a client (before upgrading
the browser, so a refusal is still an HTTP answer). Runner frames pass unchanged, and so do close
codes in both directions. From the browser only the permission mode (`{k:"mode"}`) and permission
answers (`{k:"perm"}`) pass; a message, interrupt, model or effort sent on the socket is answered
with an `err` frame naming the API call to use, and anything else with an `err` frame too, so input
reaches the runner only through Mainloop's record. Permission answers stay on the socket until they
are paired with the session's requests. It reaches the runner through the Substrate router, like the workspace preview proxy and over the same mutual-TLS
connection (see "Router TLS" in [workspaces.md](workspaces.md)). `live` is true when the
session has a kagent Session and its Agent is listed in `MAINLOOP_LIVE_AGENTS`. An attach is the
owner's only (another owner's session, or one that is not live, is `404`); a cross-origin `Origin` is
`403` and a request that is not a WebSocket upgrade is `400`. The relay names the actor the way the preview
proxy does: from the kagent Session's runtime association when kagent reports one, current or not,
then from the Session's A2A authority, and otherwise by kagent's stable Actor name (`session-<kagent session id, lower case>` in
`KAGENT_ACTOR_ATESPACE`). It does not wake anything itself: its connection through the router wakes
a sleeping Session, so it connects to one only with `wake=1`; without it, a Session whose runtime
association kagent reports as not current is `409` and nothing is dialled. It answers `503` with
`Retry-After: 2` while the tunnel is not up yet or the Session is not `READY`. A session started from
`/agents` (Start session) is a standalone native session. The server bootstraps the
runner on first attach to each actor (`PUT /bootstrap`, trust on first use; `409` means already set)
with a token derived from `AGENT_TOKEN_KEY` and the kagent Session id, adds it to the upstream
request, and strips cookies, credentials and `Origin`; the token never reaches the browser.
`?since=<seq>` is passed through. A session created before its Agent ran the SDK runner has nothing
listening on `:7682`: it still reports `live: true`, and the attach is `502` (the view then shows the
unavailable state above).

Proven live on mainloop-dev (2026-11-10) on a new `claude-workspace-dev` session: the upgrade, the
runner's `hello` backlog, and use from a phone, through the gateway, the dev frontend's proxy and the
relay. That proof was on the previous kagent API (Substrate 0.4), where the relay woke a sleeping
session with a resume call. On the current API (`kagent.api.v1`, Substrate 1.0) the relay is tested
with fakes only: its bootstrap, refusals and not-ready answers (a fake runner and targets), and the
actor naming for a sleeping Session (the fake kagent). A workspace
session's live view has not been run on mainloop-dev.

Opening a session follows its URL. Missing sessions show a not-found state. If the backend is unavailable, the page retries instead of treating the session as missing.
An HTTP 5xx on session or workspace detail shows a server error with an instruction to reload,
separately from a failed connection or a 404.

## Evidence boundary

Measured on the kagent spike cluster (live, 2026-10-04): re-sending a `messageId` whose task had completed started a second task, so Mainloop relies on never re-sending, not on kagent deduplication. The kagent client and delivery handling are tested against a fake A2A and SessionService gateway with sanitized fixtures (`server/internal/kagent/testdata`). These are fixture tests, not live proof.

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

Existing sessions are not migrated, and there is no legacy workspace enrollment or transcript
conversion. The task-session checks have fake runtime and PostgreSQL evidence only;
there is no live native-agent, Kubernetes or retained-source handoff qualification in this slice.

## Forked sessions (issue #311)

A session forked with `POST /checkpoints/{id}/fork` ([warm-state](warm-state.md#checkpoint-and-fork-api-implemented-experimental))
always gets a Mainloop session row with `parent_session_id` set. If its source session is itself a
workspace (a git checkout, not every session has one — a plain `/agents` session or the main
thread does not), the fork also gets a workspace row, so it is a first-class workspace session
like any other: its manifest (repository, ref, branch, depth, agent kind, dev settings and
development environment) is copied from the source, and `GET /workspaces/{fork}` works, as do
refresh and delete, through the same code paths as any workspace (see
[workspaces](workspaces.md#forked-workspaces-issue-311)). Which checkpoint and source workspace a
fork came from is not duplicated onto the new row: it is the existing `checkpoint_forks` row (keyed
by the fork's request id) together with the forked binding's `parent_session_id`, which already
name the checkpoint and the source session (and, since a workspace's row shares its session's id,
the source workspace). A fork of a non-workspace session gets no workspace row; there is no
manifest to copy and this change does not invent one.

A fork's agent binding keeps `mcp_grant_kind` `none` and is issued no MCP credential, the same as
before this change: kagent's `ForkSession` carries no credential reference for Mainloop to attach
one to (`checkpoints.proto`'s `ForkSessionRequest`, and `sessions.proto`'s `Session` comment that
forks do not inherit credential bindings; see warm-state.md's measurement). This is why the fork's
MCP calls are still rejected (401 `AUTH_HEADER_REJECTED`) where the source session's succeeded, and
it is deliberate, not an oversight left over from the fork having no workspace row: giving the fork
a credential anyway, by some path other than the one every other session's credential is issued
through, would be a workaround this change does not take. The grant staying `none` is how that
state is recorded; it is the same field an ordinary ungranted `/agents` session already carries,
read here as "this session has no Mainloop credential yet" rather than "this session never asked
for one". A kagent-side change (`oldsj/kagent`, branch `mainloop-substrate-0-4`) adds credential
references to `ForkSessionRequest`; a follow-up Mainloop task vendors it and issues per-fork
credentials once it lands and the vendored pin is bumped.

Tested against the fake kagent only (a fork of a workspace gets the workspace row with the copied
manifest; a fork of a plain session does not); not measured against a live kagent. See
[workspaces](workspaces.md#forked-workspaces-issue-311) for the delete behavior and
[checkpoints](warm-state.md#checkpoint-and-fork-api-implemented-experimental) for
`DELETE /checkpoints/{id}` now answering 409, not 200, while a fork still pins the checkpoint.

## React status

Labels: **implemented** means built in `web/` and covered by component or route tests against a fake backend. Nothing here is live-proven.

- **Implemented:** session rows (on the project page); the `/agents` form, reachable by URL only (Claude Code or Codex, Ctrl/Cmd+Enter submits, blocked while offline); and the session page. The page offers **Stop turn** only while the native state reports a turn in flight, and never on archived, failed or cancelled sessions, **Cancel session** through an in-app confirmation (never `window.confirm`), and **Clear** for completed, failed and cancelled sessions, which returns to the main chat. It also shows the task link, workspace badges, summary or error, the identity strip, and the not-found, server error and unreachable states.
- **Implemented:** after any session action, the page re-reads the session, its native state and attention before it shows the result. A `409` therefore shows the current state.
- **Implemented:** the turn recovery panel in every chat (main thread and session chat). While a turn is in flight it shows for a stalled turn (how long it has been quiet, with **stop**, and **restart session** when the session is `restartable`), and for a restartable session whose last stop failed (it says the turn could not be stopped and offers **restart session**). The in-app confirmation (never `window.confirm`) says the native context is lost, the open turn ends as failed and is not sent again, and the next message starts a new native session with the recent conversation. One request id is kept per restart until it succeeds, so a retry after a lost reply or a `502` continues the same restart. Tested with MSW only.
- **Implemented:** while the session page first loads, placeholder rows with a row's height sit under the loading text. The text is what a screen reader announces; the placeholders are hidden from it.
- **Wording:** the identity strip's live word is the agent's own state, shown verbatim. It shows `live` when the agent is ready, otherwise whatever state string the agent reports (for example `busy` or `suspended`, and any other word it sends), or `unknown` when there is no state yet. A failed or unconfirmed last delivery takes priority and shows as `last message failed` or `last message unconfirmed`, so it never reads `live`. A cancelled session shows `cancelled` the same way, even when the agent still reports ready. When the backend cannot be reached the word is `unreachable`, which the page adds itself; an agent never reports it.
- **Implemented:** the session chat in the page's chat slot (`SessionChatSlot`). It shows the session's own conversation, with the same timeline, delivery notices, retry and composer as the main thread. It reads `GET /sessions/{id}/native` and `GET /sessions/{id}/conversation`, and sends to `POST /sessions/{id}/message`. Its unsent draft is kept for that session only. While a turn is in flight the header shows the turn's state and a **stop** button. A cancelled session's header reads `cancelled`, never the agent's own `ready`, and the **stop** button is hidden when the native state is unknown or malformed. Its pending requests show as cards at the end of the conversation (below); it does not show the topic index.
- **Implemented:** a closed session (archived, failed or cancelled) locks its chat composer and says why in the chat panel: "This session is archived, so it takes no new messages.", or the same for failed or cancelled. **Stop** is hidden there too. Completed standalone and adopted scheduled sessions stay open. In the Go server, a stopped task-owned session rejects sends under the permanent fence above even when its activity status is completed. The chat takes the rest of the page below its header. Its message list scrolls inside it, and the composer stays at the bottom edge, visible without scrolling at phone width (390×844). The chat has no fixed share of the viewport, so a soft keyboard that shrinks the page shrinks the list, not the composer's position. The page follows its container, so the bottom tab bar and an offline banner that appears later stay clear of the composer; the visible viewport only caps it, for the keyboard. Pending request cards sit at the end of the conversation and scroll with it, so the chat slot has one scroll, and the chat panel keeps a floor for its composer.
- **Unreachable:** the page recovers in two ways. When the connection comes back, the shared live-updates module (`web/src/shell/LiveUpdates.tsx`) refetches open queries without a click. The state also offers a manual **Try again** button.
- **Not built here:** the notification toast.
- **Not proven:** the native poll while a turn is in flight (not cheap to test with MSW), and any run against a live backend, the session chat included. The chat's fill and composer position are tested with the layout classes and a mocked visual viewport only; they have not been checked on an iOS device or in a phone-width browser.
