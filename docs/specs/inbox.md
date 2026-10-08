# Inbox

The inbox shows native structured-input requests alongside existing notifications and questions.
A `hitl_request` item references the durable HITL projection. Inbox and chat mount one shared
renderer and use the same response client and `/hitl/{id}/respond` route. The inbox refreshes
while mounted and visible so discovery does not depend on a browser being present when a request arrives.

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

Polling pauses when the document is hidden and refreshes on return. Inbox/chat mounts share
one poll per request ID; delivered receipts stop automatic polling. Pending and uncertain
requests continue reconciling. UI polls share one in-flight read and start at most four reads
per second; busy lists can therefore refresh less often than the nominal four seconds.
Reads have a ten-second deadline. Hidden/unmounted reads are cancelled; a transport that has
not settled retains its lock. Inbox reads from polling, SSE and manual refresh also coalesce
behind one in-flight request. Poll cleanup does not cancel decision submissions.

## Coding-task attention links (source implementation; qualification pending)

A delegated leaf's pending input links to the existing canonical HITL inbox card. Its parent
and root task roll up that same card ID as presentation only. Duplicate observations create
no additional owner card or task attention event. The existing native leaf identity,
continuation destination and immutable receipt retain ownership; a parent link is not a
response route and cannot grant merge consent.

The observer refreshes task links after committing its HITL observation, releasing receipt
locks before taking task-tree locks. Leaf/root projection updates then commit together.
Already-recorded leaf receipts, superseded input and stale merge proposals are excluded from
pending task attention. Changed-head observation clears the old proposal/approval links.
The shared response/read integration must call the same refresh helper after receipt recording
so those links clear promptly without waiting for another native observation. That shared
wiring remains unapplied in this source slice. Offline and PostgreSQL regression sources are
prepared, with execution reserved to the heavy-check lane; no live-provider proof is claimed.
