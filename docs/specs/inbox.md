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
