# Inbox

The inbox defaults to **Needs you**, read from `GET /attention`. Its badge counts unresolved
owner actions, including requests the owner has already read. Reading a request does not settle it.
The response includes the session-list projection from that same database snapshot; a foreground
refresh updates the badge, attention items and session labels together.
The server projects current native permission requests and questions, pending approval/review
cards, and tasks explicitly blocked on an owner decision. Each item states what is needed and
links to its canonical input card or task. Idle sessions, CI waits, publication updates and
ordinary reconciliation do not imply owner attention. An empty inbox says **Nothing needs you**.

Needs you holds only items with a real owner decision (kind `approval`, `question` or
`owner_decision`). A stall, an unresolved runtime stop, or any other `task_reconciliation` item is
Mainloop's own recovery work, not yet a decision: it opens with kind `problem` instead, is left out
of the `count` the Needs-you badge shows, and is listed in the separate **Problems** view
(`GET /attention`'s `items` still carries it; the explorer's Problems group and the Inbox's
Problems tab are both filtered reads of the same snapshot). A problem escalates into a real
`owner_decision` item only once automatic recovery has failed and a concrete choice exists; that
escalation is not built (see [task-handoff](task-handoff.md) for the retry/reassign work it
depends on).

An unresolved owner decision is stored separately from native input status. If a session also
asks a native question or permission, both requests remain visible; settling the native
request alone cannot hide the owner decision. Attention reads include every eligible pending
request, independently of the history limit. Activity reads select at most the latest 100
records in SQL; polling the attention snapshot does not load Activity history.

Native input must have an unanswered verified leaf on the current live binding. Terminal tasks,
noncurrent or finished attempts, archived attempts/sessions, deleted or replaced native sessions,
superseded requests and recorded decisions never count. Parent task rollups reuse the canonical
leaf card; aliases do not add badge items. Decisions whose transport is pending or uncertain
remain inspectable through the existing session/HITL receipts, without soliciting a second answer.

The quieter **Activity** view uses `GET /activity` for FYIs such as merged PRs, provider reports
and unavailable input notices. Activity does not contribute to the badge. Provider reports are
labelled unverified and link to the task; they are not proof of completion.

A `hitl_request` item references the durable HITL projection. Inbox and chat mount one shared
renderer and use the same response client and `/hitl/{id}/respond` route. Foreground attention
polling runs even when the inbox panel is closed, so discovery and the badge do not depend on
the panel being mounted when a request arrives. Polling and SSE refresh the same projection.

Tool calls show exact names, native call IDs and arguments. Each call needs an explicit decision;
a complete batch is submitted together. Questions require all answers and respect single/multiple
choices or free text. Malformed/unsupported requests show no actionable controls. Agent notes
and unknown fields are inspectable, escaped text, never merge evidence.

Writes default off. Disabled writes, stale observations, unavailable requests, selected alias
routes and decision delivery uncertainty each have visible states. One browser shares the
in-flight state across mobile/inbox/chat mounts. Other-device conflicts refresh the immutable
server receipt. Network/5xx submission failures do not offer automatic or contradictory retries.
Read/unread remains presentation state; generic queue responses cannot settle HITL.

An accepted response is marked responded and leaves the pending inbox on refresh. The page that sent
it keeps a one-line receipt in place until reload, even after the server stops listing the request.
Chat retains current read-only receipts until superseded by newer input. Decision delivery never implies task
completion. Supported provider reason behavior is documented in [chat](chat.md#native-structured-input).

Raw queue list and legacy count reads retire pending HITL cards whose verified leaves all belong
to completed, failed or cancelled tasks, including unavailable input and uncertain decisions.
Unavailable projections without leaves use the observer's verified session binding. Mixed
active/terminal sources and unknown bindings remain available in raw queue history. The Needs
you projection only includes verified current live sources. Retired cards have queue status
`expired` and remain available through `/queue?status=expired` and the individual item route;
request snapshots, aliases, decisions and delivery state are unchanged. This is presentation
cleanup, with no native response or delivery retry.

Expiry serializes with observer request/card writes and checks current sources after locking.
Observer refresh and pending list/count use the same terminal-source rule: unchanged terminal
sources cannot restore pending presentation; a verified source that becomes active can reopen
the card on observer refresh. Accepted decisions remain responded.

An input notice without a structured request reference is shown in Activity. The owner can
expire it through `POST /queue/{id}/dismiss`; structured requests cannot use this route.
Dismissal retains the notice for audit and records no approval or rejection.

Polling pauses when the document is hidden and refreshes on return. Inbox/chat mounts share
one poll per request ID; delivered receipts stop automatic polling. Pending and uncertain
requests continue reconciling. UI polls share one in-flight read and start at most four reads
per second; busy lists can therefore refresh less often than the nominal four seconds.
Reads have a ten-second deadline. Hidden/unmounted reads are cancelled; a transport that has
not settled retains its lock. Inbox reads from polling, SSE and manual refresh also coalesce
behind one in-flight request. Poll cleanup does not cancel decision submissions.

## Coding-task attention links

A delegated leaf's pending input links to the existing canonical HITL inbox card. Its parent
and root task roll up that same card ID as presentation only. Duplicate observations create
no additional owner card or task attention event. The existing native leaf identity,
continuation destination and immutable receipt retain ownership; a parent link is not a
response route and cannot grant merge consent.

The observer refreshes task links after committing its HITL observation, releasing receipt
locks before taking task-tree locks. Leaf/root projection updates then commit together.
Already-recorded leaf receipts and superseded input are excluded from pending task attention.
An unanswered native merge retry remains task attention even when its proposal is stale,
expired or blocked: the task stays Waiting for approval and the shared HITL card explains its
staleness. Changed-head observation clears the old merge proposal but preserves native input
links. Terminal merge evaluation recovers unanswered native links dropped by an older cache.
Response submission refreshes each unique canonical leaf binding after the durable receipt
commits and leaf/merge decision locks are released. Same-action replay refreshes those links
without recording another decision or sending another native response. Attention failure or
timeout preserves the existing receipt and continuation path.

The existing response reconciler repairs missed attention updates from committed receipts,
including accepted or rejected transport, in a separate bounded share. Cached task-tree links
identify trees to refresh even if the native request projection has been removed. If the leaf
has completed before refresh, the helper authenticates an active ancestor and recomputes its
tree. The terminal leaf keeps its terminal status and cannot submit work; its parent gains
no leaf receipt or consent. Local PostgreSQL/fake-upstream regressions cover this integration;
they provide no live-provider proof.

## React status (F5)

Labels: **implemented** means built in `web/src/features/inbox` and covered by component, model or
poll tests against MSW fakes. Nothing here is live-proven.

History before `e776537` describes the retired Python backend. Where other text on this page differs from the Go status, the Go status is current.

- **Implemented:** the **Needs you** list from `GET /attention`, filtered to real owner decisions (kind != `problem`). A HITL row shows the shared request card from `features/hitl`. A request this browser answered stays as a card with its receipt, in the same place in the list, until reload, even after the server stops listing it. Focus stays on that card. Other queue rows show option buttons, a free-text reply for open questions and plan reviews, a folded plan, and a **View PR** link only for `https` URLs. A row without a queue item links to its task. The empty state says **Nothing needs you**.
- **Implemented:** the **Problems** list, the same `GET /attention` read filtered the other way (kind == `problem`): a stall or an unresolved runtime stop, with a plain-language explanation and a **Review task evidence** link, never a decision's buttons. The empty state says **Nothing here**.
- **Implemented:** the **Activity** view from `GET /activity`, read only while it is open. Provider reports keep the server's `unverified` label and link to their task.
- **Implemented:** **Dismiss notice** on an Activity row the server marks `dismissible` (an input notice with no structured request reference), through `POST /queue/{id}/dismiss` with the row's `queue_item_id`. The client does not copy the rule. The row leaves Activity at once. A refusal, including a `409`, restores the row and shows the server's reason above the list. That reason clears once the re-read removes the row. Both Needs you and Activity re-read once the request settles, so the owner sees the current state.
- **Implemented:** the count covers unresolved, real-decision attention only (never a problem) and reads `99+` above 99. Attention polls only while the page is visible, one read at a time, on the shared cadence and deadline. `inbox:updated` refreshes attention and activity.
- **Implemented:** there is no Inbox tab and no inbox badge. The count is on the explorer's **Needs you** group (the Explorer tab on the phone, the side bar or its collapsed rail on desktop), and the Explorer tab's accessible name includes it. A separate **Problems** group sits just below it, counting only problems, muted rather than attention-coloured. The explorer reads attention, so it keeps polling while the chat or another page is open.
- **Implemented:** the answer view at `/inbox` (`?show=activity` for Activity, `?show=problems` for Problems, `?item=<request or queue item id>` for one row). It replaces the chat in the centre column. The explorer's **Needs you** and **Problems** groups and each of their rows, the waiting row under **Since you were here**, a task's **Open Inbox** link and an old `/?tab=inbox` link open it. The chosen row is brought to the top of the view and marked, and it is brought back to the top while its card fills in, until the owner scrolls, taps or types. The view's header (the back link, **Needs you**, **Problems** and **Activity**) stays at the top while the list scrolls. The view has one scroll: the page's own.
- **Implemented:** a task page whose current attempt holds an open `owner_decision` leads with the ask (the report's `owner_action`, `Task.owner_decision.action`) and the actions that answer it (`features/tasks/TaskActions`), before the task's facts and history, which sit behind a collapsed **Task details** disclosure. A task with no open owner decision keeps the plain, uncollapsed layout.
- **Implemented:** text on a card grows with the page instead of scrolling inside the card. Tool arguments and a captured PR description longer than 12 lines or 1,200 characters show their start and a **Show all** toggle; request data and a plan are already folded behind their own toggles and show whole when opened.
- **Implemented:** after **Send response**, the card keeps a receipt line built from the response this browser sent, such as "Approved Bash, rejected Edit · 6:20 PM". The time is when this browser sent it, since the server receipt has none. Focus moves to the receipt once it is recorded. While the send is in flight, and when it is uncertain, focus stays on the card heading, never on the page body.
- **Implemented:** the session page (`features/sessions/SessionChatSlot`) shows the session's pending requests as cards at the end of the conversation, in the same scroll as the messages. Its state reads **needs you** while a request is pending, in place of working, ready or idle. Failed and unconfirmed delivery still show as such.
- **Not built:** answered requests in **Activity**. The server's `/activity` query reads queue items only (`source_kind = 'queue'`), so an answered HITL request is not returned. The receipt lasts only for this page visit; the server must return answered HITL items before Activity can show them.
- **Not built:** the notification toast. The plan drops it (production never creates notification rows, and the inbox covers attention), but F4's result assigned it to F5. The supervisor needs to settle this.
- **Not built:** the retired SvelteKit app's `?inbox=<id>` parameter. The deep link is `/inbox?item=<id>`.
- **Not built:** skeleton loading, and the **Last checked** and offline timestamps. Loading shows a status line. A failed refresh keeps the rows already shown.
- **Not proven:** the 390 px layout of the request card and merge SHAs, which the vitest tests do not measure. The Playwright journeys for inbox (the answer view, Needs you, Activity, dismiss at phone width), deferred to F9 (`web/e2e/**`). Also not proven: live SSE delivery, poll timing in a real browser, and any run against the Go server.

## Go HITL observer and response dispatch

Status: implemented in the Go server (G9a, `server/internal/hitl`, migration `00070_hitl`), with
tests against an in-memory kagent. Nothing here has run against a live kagent.

Built:

- **Observer.** A sweep on `reconcile.Runner` lists each bound session's waiting kagent tasks
  (filtered to `input-required`, each with only its latest message) read-only, keeping a
  task-cursor checkpoint per session. Each session is asked on the Agent that runs it, resolved by
  the same rule the session routes use (`sessions.Service.ResolveAgent`: the workspace's Agent,
  else the main Agent for the main role, else the provider profile's) and stored on its
  `hitl_sessions` row; a row whose binding no longer names its kagent session is deleted. A scan
  that fails for one session (credentials or wrong-Agent refusal, a lost reply, a timeout, a bad
  reply) records a diagnostic and backs that session off (30 s, doubling to 10 min) while the
  others go on; an unreachable kagent, or a refusal of our own bearer (HTTP 401, `UNAUTHENTICATED`), ends the
  scanning for that pass and backs nobody off. Staleness never comes from
  a scan: it comes from each request's own refresh failing. A task in `input-required` whose status message
  carries the HITL extension becomes a `hitl_requests` row (`pending`), identified by a hash of
  the session, task, status message and request body. Each open request gets an `attention_items`
  row, and `inbox:updated` is published in the same transaction. A request appears within one scan
  interval, or within one observer sweep (5 s) when a delivery turn parks in `input-required`:
  the delivery marks the session due (`hitl_sessions.scanned_at` cleared) and does nothing else.
  Once kagent accepts an answer, the dispatcher also makes that task's delivery due now and notifies
  the delivery loop, so the delivery reads the resumed task at once: it hints for the next ask, or
  mirrors the turn's final reply, instead of waiting out its parked backoff. The mark
  coalesces; it skips a task that already has a live request, it does not shorten a session's failure
  backoff, and a lost mark or wake falls back to the scan interval and the delivery's parked
  backoff (`auth-required` parks are not hinted).
- **Availability.** `pending` becomes `answered` when the task shows the response message,
  `unavailable` when the task moves on or disappears or a newer ask replaces it, and `stale`
  (not answerable) when its own refresh has not been confirmed by kagent for two minutes. A stale request reopens when
  kagent confirms it again. Attention rows resolve when a request leaves `pending`/`stale`.
- **Malformed asks** (no tools, a bad question list, an oversized payload) create no card; they
  appear as diagnostics on `GET /hitl-observer/status`.
- **Respond.** `POST /hitl/{id}/respond` validates the answer against the stored ask (every
  top-level tool decided, question ids and counts matching, answers within the choices), then
  writes a `hitl_responses` intent row keyed by the action id. The same action id with the same
  answer returns the same receipt; a different answer or request is `409`. One live response per
  request. `action_id` is 1 to 2048 characters. A nested (delegated) request is shown with
  `answerable=false` and a reason, and answering it is `409`, whatever the answer says, until G10 implements the leaf rule. An attention
  item is still opened for it, so the owner can see that a delegated agent is blocked, but its text
  says so ("A delegated agent is waiting (cannot be answered here yet): Approve …"); the card
  beneath it carries the reason and no controls.
  The view of a request whose latest response was `rejected_transport` has `response: null`, keeps
  `transport_state`, and says why in `transport_detail`; `transport_detail` is also set for
  recorded, sending and uncertain responses that have a reason. With `MAINLOOP_OWNER_HITL_WRITES_ENABLED` off the route answers `503`.
- **Dispatch.** A reconcile loop reads the task first and sends only while the task still asks
  for exactly this request. Before the send it makes the Session ready as deliveries do: it wakes the
  workspace and resumes a suspended Session (`sessions.Service.ReadySession`), so an answer to a
  suspended workspace wakes it. Waking is not a send: while the Session is not ready the response
  stays `recorded` and is tried again after a minute. kagent's refusal of a send to a Session that is
  not ready counts as not sent. The row is marked `sending` and committed before the call, and no
  transaction is open across kagent I/O. A send that provably did not leave is retried; any other
  failure records `uncertain`. When the send timeout passes, the send is stopped before the dispatcher
  decides: a refusal that was only being retried (kagent's "not ready" or "not accepted") ends as not
  sent and no later retry can deliver it, while a request that may have reached kagent stays
  `uncertain`. An uncertain response is never resent: later passes look the
  outbound message id up. Only the original task can accept it: its history must show the id with
  the HITL metadata that was sent. A message that only another task shows is
  `rejected_transport` ("kagent took the answer as a new turn"), as is one whose metadata differs.
  The lookups back off from 15 s, doubling to 1 h, but never past the end of the hold. After ten
  minutes with no message, the response
  becomes `rejected_transport` whether the ask still waits or the task moved on, and the owner may
  answer again.
- **Inbox.** `inbox:updated.unread_count` counts open attention items that are unread.
  `GET /sessions/{id}/hitl` lists the newest 100 requests, oldest first within them, so a
  conversation reads top to bottom and the cap drops the oldest.
- **Routes.** `GET /sessions/{id}/hitl`, `GET /hitl/{id}`, `POST /hitl/{id}/respond`,
  `GET /hitl-observer/status`.

Not built:

- Whether the harness's parked turn survives a suspend of the workspace. A suspended workspace is
  woken by the answer, but it is not known that the agent's waiting turn is still there to take it
  (the harness reports "runtime has no parked turn to continue" otherwise). This needs a live
  check. If it does not survive, the fallback is to count an open ask as workspace activity so an
  idle workspace with an ask is not suspended; that costs a running pod, so it is the owner's
  decision.
- Proof against a real kagent. The production sender (the shared client, with the answer in the
  HITL extension metadata and the `A2A-Extensions` header) is tested only against the in-process
  fake.
- Answering nested delegated requests (shown, not answerable) and task-tree attention links.
  Merge details are served by the Go merge slice; the Go card context is described below.

## Go attention, activity and queue (G9b)

Status: implemented in the Go server (`server/internal/attention`), with tests against scratch
PostgreSQL. Nothing here has run against a live cluster.

- **Items.** One `attention_items` row per thing that needs the owner. Sources: `hitl_request`
  (from the HITL observer; item id `hitl:<request id>`), `task_decision` (from `tasks.decision_*`;
  id `task:<task id>`) and `queue` (questions other slices open with `attention.Open`; id
  `queue:<id>`). A sweep opens a decision item for each task that is `blocked` for `owner_decision`
  on its current, unfinished attempt, and resolves it when the decision clears, the attempt is
  replaced or the task finishes. A new ask from the same task is a new item; an answered one is
  never reopened.
- **Kind.** `approval`, `question` and `owner_decision` are real owner decisions; `problem` is
  Mainloop's own recovery work (every `task_reconciliation` item; see A1 below), with no decision
  yet for the owner. `AttentionSnapshot.count` counts only the former, so a problem never raises
  the Needs-you badge even while it stays open and listed.
- **Visibility.** One rule (the `attention_visible` view) decides which open items the owner
  sees; the list, `count`, the unread count and `needs_owner` all read it. An item is hidden when
  its session is archived or finished, when a HITL request already has a live answer (recorded,
  sending, accepted or uncertain), when its task is final, or when its attempt is not the task's
  current attempt, or is archived or its native session deleted. The sweep (every 3 s) also closes
  the items that are hidden for good: those of finished or archived sessions, of a final task, or
  of an attempt that was replaced, archived or deleted. A request hidden only by an answer stays
  open, so a response that ends `rejected_transport` shows it again.
- **inbox:updated.** Every write that changes the unread count publishes it in the same
  transaction, including a response that ends `rejected_transport`. Moves with no write of their own
  (a session or task ends) are caught by the sweep, which compares the count with the
  `unread_count` of the owner's newest `inbox:updated` in the events table, never with process
  memory: a restart or a second replica neither misses nor repeats an event, and a missing or pruned
  event means publish. The sweep also publishes when it closes an item. Two sweeps in the same
  instant can each publish once.
- **needs_owner and sessions.** A session needs the owner when it has a visible item. The
  snapshot's `sessions` is the owner's session list as `/sessions` gives it (every unfinished
  session plus the newest 100 finished ones, not the main thread), each row with its own
  `needs_owner`; a session reads `waiting_on_user` only while a native input request waits on it.
  Items, count and sessions are read in one read-only transaction. `/sessions` still reports
  `needs_owner: false` until the sessions package calls `attention.Service.NeedsOwner`; project
  detail already does.
- **Count.** `count` is the number of open visible items. `inbox:updated.unread_count` is the open
  visible items not yet read; answering a queue item reads it. Activity never counts.
- **Activity.** Provider reports ("Provider report (<outcome>; unverified): …", id `report:<id>`,
  not dismissible), answered queue items (`queue:<id>`, not dismissible) and input notices
  (`queue:diag_<n>`, `queue_item_id` `diag_<n>`, dismissible), newest first, at most 100. A question
  the sweep closed with no answer reads "Closed: …", not "Answered". Queue ids cannot start with
  `diag_`.
- **Respond.** `POST /queue/{id}/respond` answers an open `queue` item: the answer is stored on the
  row, the item resolves and `inbox:updated` is published in the same transaction. The same answer
  again is `200` with no second event; a different answer, or an item that is no longer open, is
  `409`. HITL items answer `409` ("use the structured HITL response route"), notices `409`, unknown
  ids `404`. The answer is not delivered anywhere yet: the slice that opened the item reads it.
- **Dismiss.** `POST /queue/{id}/dismiss` puts an input notice away (idempotent; no event on a
  repeat). Native requests and queue items answer `409`, unknown ids `404`.
- **Not built.** `403` (there is one owner), queue expiry, FYI notification rows, and delivery of
  an answer to the main thread.

## Reconciliation and merge attention (A1)

Implemented in the Go server with scratch PostgreSQL and fake-runtime tests; no deployed proof.

- The attention sweep opens one `task_reconciliation` item per task (`reconciliation:<task id>`)
  for `blocked/reconciliation`, `blocked/session_lost`, `blocked/stalled`, `blocked/runtime_limit`,
  or for any attempt carrying `runtime-stop-unresolved` evidence whose durable stop state is not
  `confirmed`, as long as the task itself is not yet final. Its kind is always `problem`: none of
  these is an owner decision on its own, so none counts toward Needs-you or shows attention
  yellow; escalating one into a real decision needs automatic recovery to fail first, which is not
  built (see [task-handoff](task-handoff.md)). Its closed `reason_code` is `reconciliation`,
  `session_lost`, `stalled`, `runtime_limit` or `runtime_stop_unresolved`, with the unresolved stop
  taking precedence. The item resolves, and is never (re)opened, the moment the task reaches
  `completed`, `failed` or `cancelled`, even while its stop remains unresolved: the task is over, so
  there is no decision left for the owner, whatever claim or capacity the attempt still holds behind
  the scenes. While the task stays open, an ended session or a replaced attempt does not by itself
  resolve the item: it still needs this item while its stop remains unresolved. Confirmation clears
  the stop need without deleting historical evidence; an independent reconciliation block continues
  to need attention. The item resolves when both conditions clear (or the task finishes), and
  reopens under the same identity if the need recurs. It links to task evidence and grants no new
  task action.
- The HITL observer binds merge calls to the runtime's named template and configured provider/tool,
  then stores their owner/session/project-scoped proposal associations. The attention sweep opens
  one `merge_proposal` item per approval-route proposal (`merge:<proposal id>`, reason
  `merge_approval`), linked to its canonical HITL request. The card remains a structured HITL
  response, not an ordinary queue answer. While a proposal item is open, it suppresses the generic
  native inbox duplicate for the entire request. Once the last proposal item resolves, the generic
  item reappears for any still-unanswered native input, including pure merge requests and mixed
  batches. Only the native request's own response and finality rules can close that remaining need.
- Recording approval or refusal resolves proposal attention, as do supersession, expiry, a passed
  deadline, a request becoming unavailable, or the candidate leaving `prepared`. A response rejected
  by transport can reopen attention while the same proposal still waits. Final or archived sessions
  and obsolete task attempts cannot reopen merge approval items. A candidate can also stay
  `prepared` forever once its pull request is merged or closed outside Mainloop's own merge flow,
  since nothing claims a `prepared` row to ask GitHub again on its own; the sweep instead resolves
  the item once any task's own, independently polled `pr_state` for that exact repository, PR
  number and head reads `merged` or `closed`, with no GitHub call of its own. The head must match
  too, so a stale snapshot from before the pull request was pushed to and reopened cannot resolve a
  later, unrelated proposal for the same PR.
- Both lifecycles write only on a changed need or association. Repeated unchanged sweeps create no
  duplicate rows, change no item timestamps, and publish no extra `inbox:updated` events.
