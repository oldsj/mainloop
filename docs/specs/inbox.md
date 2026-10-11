# Inbox

The inbox defaults to **Needs you**, read from `GET /attention`. Its badge counts unresolved
owner actions, including requests the owner has already read. Reading a request does not settle it.
The response includes the session-list projection from that same database snapshot; a foreground
refresh updates the badge, attention items and session labels together.
The server projects current native permission requests and questions, pending approval/review
cards, and tasks explicitly blocked on an owner decision. Each item states what is needed and
links to its canonical input card or task. Idle sessions, CI waits, publication updates and
ordinary reconciliation do not imply owner attention. An empty inbox says **Nothing needs you**.

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

An accepted response is marked responded and leaves the pending inbox on refresh. Chat retains
current read-only receipts until superseded by newer input. Decision delivery never implies task
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

- **Implemented:** the **Needs you** list from `GET /attention`. A HITL row shows the shared request card from `features/hitl`. Other queue rows show option buttons, a free-text reply for open questions and plan reviews, a folded plan, and a **View PR** link only for `https` URLs. A row without a queue item links to its task. The empty state says **Nothing needs you**.
- **Implemented:** the **Activity** view from `GET /activity`, read only while it is open. Provider reports keep the server's `unverified` label and link to their task.
- **Implemented:** **Dismiss notice** on an Activity row the server marks `dismissible` (an input notice with no structured request reference), through `POST /queue/{id}/dismiss` with the row's `queue_item_id`. The client does not copy the rule. The row leaves Activity at once. A refusal, including a `409`, restores the row and shows the server's reason above the list. That reason clears once the re-read removes the row. Both Needs you and Activity re-read once the request settles, so the owner sees the current state.
- **Implemented:** the count covers unresolved attention only and reads `99+` above 99. Attention polls only while the page is visible, one read at a time, on the shared cadence and deadline. `inbox:updated` refreshes attention and activity.
- **Implemented:** the badge (`InboxBadge`) is mounted on the compact **Inbox** tab in `CompactShell.tsx`, beside the owner-task badge. Attention is read by the shell, so it keeps polling while another compact tab is open. On desktop the count is on the **Needs you** tab, in the always-mounted panel.
- **Not built:** the notification toast. The plan drops it (production never creates notification rows, and the inbox covers attention), but F4's result assigned it to F5. The supervisor needs to settle this.
- **Not built:** the `?inbox` URL parameter from the frozen app. Opening the inbox is local state, with no deep link.
- **Not built:** skeleton loading, and the **Last checked** and offline timestamps. Loading shows a status line. A failed refresh keeps the rows already shown.
- **Not proven:** the Playwright journeys for inbox (tabs, badge, Needs you, Activity, dismiss at phone width), deferred to F9 (`web/e2e/**`). Also not proven: live SSE delivery, poll timing in a real browser, and any run against the Go server.

## Go HITL observer and response dispatch

Status: implemented in the Go server (G9a, `server/internal/hitl`, migration `00070_hitl`), with
tests against an in-memory kagent. Nothing here has run against a live kagent. The Python
observer above is frozen reference; the Go slice covers a subset of it.

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
  interval.
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
  `answerable=false` and a reason, and answering it is `409`, whatever the answer says, until G10 implements the leaf rule. A nested
  request still opens no actionable attention item yet: its card cannot be answered from the
  inbox, and G9b words it (or opens none).
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
  failure records `uncertain`. An uncertain response is never resent: later passes look the
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
- Merge details (`/hitl/{id}/merge/{proposal_id}/details`) stay on the 501 stub, as do
  answering nested delegated requests (shown, not answerable) and task-tree attention links.
