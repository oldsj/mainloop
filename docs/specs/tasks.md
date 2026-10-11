# Durable tasks

Blocked reports may include `owner_action`, a specific question or decision required from the
owner. This field is accepted only with outcome `blocked` and sets reason `owner_decision`.
It also stores a separate durable `owner_decision` with the report ID, attempt ID and action.
The attention projection includes that decision for the current active, unarchived attempt
until it is cleared. Native attention may change task status/reason to waiting/approval and
back without changing the owner decision. When both requests are pending, the inbox shows
both; answering the native question does not answer the owner decision. A generic
blocked/reconciliation or failed report does not create or clear an owner decision.
A progress or completed report from the same current attempt clears the decision;
task finality or replacement/removal of that attempt also clears it. Archived or retired
attempts cannot surface it. Task pages retain the reports and the existing actions and links.

Routine `progress` and `completed` reports remain durable and appear in Activity. They do not
enqueue a main-agent chat turn. Parent-task notifications retain their existing delivery;
blocked and failed reports still notify the main agent. The main agent reads routine results
through `task_get`, `task_list`, and standing task context on its next turn.

This routing also applies to a final `completed` report from an agent whose coding task was
already completed by verified merge: its report and event remain durable, with no main-thread
delivery and exactly one parent-task delivery when it has a parent. Replaying the same report
request creates no duplicate report or delivery. Recording it preserves the completed task's
status, publication projection, result reference and held capacity. The completed binding's
restricted tool scope and current authority checks still apply; Approval and protected-path
routes require the exact original owner receipt.

Implemented through S2: task/attempt/operation persistence, owner reads, project provider
preferences, transactional admission, S1 native provisioning/cancellation and durable task
notifications. MCP `delegate`, `report`, `task_get`, `task_list`, `task_history` and `task_cancel`
use shared durable task contracts and persisted role/depth/attempt authority. Main creates
supervisors; supervisors create direct children; children cannot delegate. Legacy session
`status`, `read`, `cancel`, `clear` and spawn/one-shot report schemas are removed.

Reports are unverified claims with transactional recipient intents, current-authority routing
and native queue admission. Revoked unsent recipients are cancelled at promotion/send claim
and retained for authorized successor routing. Sending/uncertain outcomes are never replayed.
Explicit coordination completion requires no live children and confirmed runtime termination.
Coding task completion still requires S4 verified publication. Retry/reassign stay unavailable
until S3's shared handoff port is installed; retention cleanup is also S3 scope. Offline fixtures
qualify deterministic behavior only, not live native/provider integration or published-head CI.

## Identity and state

A task records owner/project/topic, parent/root, creator binding, title/brief, code or
coordination mode, selected profile/source, optional inherited owner provider constraint,
status/reason, current attempt, version and timestamps. Code requires an owner-owned project
and typed branch/ref/depth. Coordination has no checkout or repository authority.

The server refuses a code task's checkout with 422 `invalid_checkout` when the branch or ref is
not a plain git name (a leading `-`, `..`, `@`, a `.lock` ending, spaces or any of `~^:?*[\`, empty
components, over 255 bytes) or starts with `refs/`, or when the depth is outside 0..1000. A brief over
16,384 bytes is refused with 422 `brief_too_long`. REST and MCP share these checks.

An omitted checkout `depth` is 0, which clones full history; a positive
value requests that shallow depth. Existing tasks keep the depth stored in their snapshot.

An omitted or empty checkout `ref` selects the repository's remote default branch. With both
`GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` enabled, Mainloop resolves that default or an
explicit branch, tag or commit through its repository-scoped GitHub App client before task
admission. The task and workspace store the full commit SHA; an unavailable ref returns `422`
with `checkout_ref_unavailable` and leaves no task, attempt, workspace or create plan. Retries
with the same request ID reuse the stored SHA even if the remote ref moves. Successors continue
using their checkpoint's verified `remote_sha`. With either flag off, refs pass through unchanged.
With the push gate enabled and the project's cached default branch empty, provisioning reads it
from GitHub before freezing the attempt's Git plan, for explicit SHAs as well as named refs; if
that read fails the attempt stays `creating` with evidence `git-hold:default_branch_unavailable`
and retries (see `push-gate.md`).
Existing task checkouts are not rewritten. The required checkout `branch` remains the feature
branch used for the writer claim and push target, independently of `ref`.

Public status is `queued`, `running`, `waiting`, `blocked`, `completed`, `failed` or
`cancelled`. Reasons distinguish awaiting child, approval, CI, publication, handoff and
reconciliation. Agent activity, delivery, workspace health, attention and publication remain
separate observations. A provider report or finished turn cannot prove coding-task completion.
Verified merged publication is the later completion gate.

A verified observation of the task's own linked PR projects both `pr_state: merged`
and `merge_state: merged`, including a merge by the parent or another actor. It outranks
an older child proposal's readiness state. Observation does not transfer proposal consent
or complete the task: completion still requires the existing merge service to settle
the task-associated intent with policy and exact-head CI evidence. A merged observation
with no such settlement can leave the task blocked and its runtime reservation held;
the parent can use `task_cancel` to dispose of that runtime safely.

Task GET authorizes the task before reading its cached publication projection. Reads stay
DB-only: CI becomes unknown when its head differs, its timestamp is missing,
future or older than five minutes. The response includes the existing publication mode and
current merge proposal/result; reading never starts a native turn or dispatches a merge.

Owner and scoped native task reads also return optional `projection.pr_description`,
collected by the existing authenticated PR refresh. It carries the validated repository,
branch, PR number, current attempt ID and exact head SHA, a UTF-8 `excerpt` of at most
16 KiB, full-body `byte_length` and SHA-256 `digest`, `truncated`, and `fetched_at`.
The excerpt stops at a complete UTF-8 character; length and digest cover the original
body, including text beyond the excerpt. An empty string is an observed empty description;
a null or unavailable body returns null. An excerpt containing NUL is unavailable because
JSONB cannot store it. Invalid UTF-8 or unpaired Unicode surrogates fail the observation;
supported text is preserved exactly.

The description is editable, untrusted GitHub text. Reviewer markers and other text grant
no approval, acceptance or merge authority. Failed or inconsistent observations clear it.
Reads mask descriptions whose repository, PR/head, branch or current attempt differs,
whose PR state is unknown, or whose fetch is future or older than five minutes.
`fetched_at` uses the collection-finish time carried by the observer, including unchanged
refreshes; database delays and generic `observed_at` cannot renew a stale description.
If an attempt or checkout changes during collection, that observation is discarded.
The final PR read must agree with the initial read's identity and body; text edited during
collection is unavailable until a consistent refresh.
A successful new-head observation replaces the body with that head's own description.
Unchanged refreshes update timestamps without task version/event churn; same-head body
edits, including edits beyond the excerpt, produce an update. Reads remain DB-only and
keep existing task scope; native reads still omit pending approval IDs.

The task detail shows available descriptions as plain text under the PR observations,
preserving whitespace and marking truncated excerpts. It never renders HTML or Markdown.
Unavailable descriptions are omitted; observed empty descriptions say “No description
provided.” The Go migration uses a fresh schema; cached descriptions default to null.

Attempts pin Claude/Codex profile ID, revision and role AgentRef independently of registry
reloads. Each has a monotonic number, native/session/binding/workspace identity, writer generation,
state, brief delivery, lineage, results/evidence and retention audit. SQL rejects routing
mutation, duplicate task/number and duplicate session associations. A deferred composite FK
requires the task's current attempt to belong to that task. Native deletion preserves audit IDs.

Roles are main depth 0, supervisor depth 1 and child depth 2. Owner workspace agents are outside
the delegated tree. Principal fields are resolved by the server, never accepted in public inputs.
Main reads/manages its owner's tree; a supervisor manages direct children in its inherited
project/root; a child reads its own task. Inactive, mismatched or superseded bindings are denied.
An owner-selected provider constraint cannot be overridden by a supervisor.

## REST

Owner routes use the configured-owner dependency, ignoring caller identity headers. MCP never
uses the owner REST listener.

| Route                                    | Implemented result                                                                 |
| ---------------------------------------- | ---------------------------------------------------------------------------------- |
| `GET /tasks?project_id=&parent_task_id=` | Array of task views within owner scope                                             |
| `GET /tasks/{id}`                        | Task, attempt history, projection, action eligibility                              |
| `POST /tasks`                            | 202, idempotent provisioning operation; admission reserves task/attempt/capacity   |
| `POST /tasks/{id}/retry`                 | 202, blocked handoff operation after authority/version check                       |
| `POST /tasks/{id}/reassign`              | Same, with explicit target profile                                                 |
| `POST /tasks/{id}/cancel`                | 202, durable cancellation operation; capacity retained until confirmed termination |
| `GET /task-operations/{id}`              | Durable operation, including step and reason                                       |
| `GET /projects/{id}/default-provider`    | Profile or null, preference version (initially 0)                                  |
| `PUT /projects/{id}/default-provider`    | Compare-and-swap preference; null removes selection                                |

Example create input:

```json
{
  "request_id": "create-1",
  "title": "Fix task list",
  "brief": "Implement the specified change",
  "mode": "code",
  "project_id": "project-1",
  "provider_profile_id": "codex",
  "checkout": { "branch": "feature/task-list", "ref": "main", "depth": 0 }
}
```

Disconnected-port action response (timestamps and digest shortened here for readability; S1 installs provisioning):

```json
{
  "id": "operation-1",
  "owner_id": "owner",
  "principal_key": "owner",
  "request_id": "create-1",
  "request_digest": "sha256-digest",
  "kind": "create",
  "task_id": null,
  "attempt_id": null,
  "state": "blocked",
  "last_confirmed_step": "requested",
  "reason": "provisioning_unavailable",
  "created_at": "2026-10-07T00:00:00Z",
  "updated_at": "2026-10-07T00:00:00Z"
}
```

Actions require `request_id`, `expected_version`, and `expected_attempt_id` (explicit null
when no attempt exists). Reassign also requires `target_profile_id`. Same owner/principal/request
and normalized payload return the same operation. Changed payload/kind/target returns 409;
stale version/attempt returns 409; out-of-scope reads return 404. Authority fields, unknown
fields and old delegation `kind`/session payload aliases are rejected with 422.

Example disconnected-port read state (S1 installs cancellation; S3 handoff remains unavailable):

```json
{
  "task": {
    "id": "task-1",
    "owner_id": "owner",
    "project_id": "project-1",
    "topic_id": null,
    "parent_task_id": null,
    "root_task_id": "task-1",
    "creator_binding_id": null,
    "title": "Fix task list",
    "brief": "Implement the specified change",
    "mode": "code",
    "assigned_profile_id": "codex",
    "selection_source": "explicit",
    "provider_constraint": "codex",
    "status": "queued",
    "reason": null,
    "current_attempt_id": null,
    "version": 1,
    "checkout": { "branch": "feature/task-list", "ref": "main", "depth": 0 },
    "created_at": "2026-10-07T00:00:00Z",
    "updated_at": "2026-10-07T00:00:00Z"
  },
  "attempts": [],
  "projection": {
    "agent_activity": null,
    "delivery_state": null,
    "workspace_health": null,
    "environment_version_id": null,
    "repository": null,
    "branch": null,
    "pr_url": null,
    "pr_number": null,
    "pr_head_sha": null,
    "pr_state": "unknown",
    "ci_state": "unknown",
    "ci_head_sha": null,
    "merge_state": null,
    "merge_proposal_id": null,
    "publication_state": null,
    "pending_approval_ids": [],
    "observed_at": null
  },
  "actions": {
    "retry": { "available": false, "reason": "handoff_unavailable" },
    "reassign": { "available": false, "reason": "handoff_unavailable" },
    "cancel": { "available": false, "reason": "cancel_unavailable" }
  }
}
```

The read shape is identical for each task status. Examples of status/reason pairs:

```json
[
  { "status": "queued", "reason": null },
  { "status": "running", "reason": null },
  { "status": "waiting", "reason": "approval" },
  { "status": "blocked", "reason": "reconciliation" },
  { "status": "completed", "reason": null },
  { "status": "failed", "reason": null },
  { "status": "cancelled", "reason": null }
]
```

Frozen handoff operation states, not an implemented S0 handoff:

```json
[
  { "state": "requested" },
  { "state": "draining" },
  { "state": "checkpoint_required" },
  { "state": "checkpoint_verified" },
  { "state": "source_fencing" },
  { "state": "source_fenced" },
  { "state": "target_creating" },
  { "state": "target_ready" },
  { "state": "completed" },
  { "state": "blocked", "last_confirmed_step": "checkpoint_required", "reason": "handoff" },
  { "state": "uncertain", "last_confirmed_step": "source_fencing", "reason": "reconciliation" }
]
```

Attempt states are `creating`, `active`, `draining`, `fenced`, `superseded`, `failed`,
`cancelled`, `completed`. Allowed moves: `creating` to `active` or `draining`; `active` to
`draining`; `draining` to `fenced`; `fenced` to `superseded`, `failed`, `cancelled` or `completed`. Capacity remains held separately until confirmed fencing/termination;
reporting alone never releases it. Unknown creates/stops/publication retain their reservation.

A stop request permanently closes the attempt's session to new messages, even if the session's
activity status is `completed`. The retained attempt's `stop_state` is the send fence: it changes
from `none` to `requested` with the move to `draining`, and stays non-`none` through unresolved
stops, confirmation and settlement. Recording a message checks it in the recording transaction;
dispatching an already-recorded message checks it before native preparation. The admission
transaction serializes with stop: a committed decision before stop permits the native send, and
a stop committed first refuses admission. Cancellation retains task-owned `sending` and
`uncertain` deliveries and their claims until reconciliation resolves them, without replay.
Stop confirmation requires no unresolved admitted delivery and a native observation showing all
tasks final, with no delivery outcome change since that observation began. A `delivered` message
has been accepted by a native task that may still be working, so a late delivery or recovery
requires another observation before releasing the branch writer claim and capacity. This does
not change ordinary standalone or adopted scheduled sessions.

Verified merged coding tasks enter automatic runtime cleanup through the existing task
reconciler. It waits for all outstanding native deliveries and held child attempts, drains and
revokes MCP/push authority, confirms deletion of the exact persisted kagent Session, then fences
and releases the writer generation and capacity. Unknown, mismatched or unsettled deletion
keeps capacity held and is retried. Confirmed deletion is reusable after a restart; missing
runtime identity is insufficient. Pending cancellation/handoff and unresolved PR, merge or Git
publication hold release. Older completed rows with held attempts recover on reconciliation
without manual SQL. Failed/cancelled held coding attempts follow the same recovery path.
Cleanup preserves stored product outcome and publication projection for owner reads and ends
the completed binding's agent status access when authority is revoked during draining.

A confirmed kagent `TASK_STATE_FAILED` for the current active attempt's first brief moves the
task to `blocked` / `reconciliation` and the attempt to `draining` on the existing dispatcher's
next pass, only when that brief remains the session's sole delivery. The native ledger records
the accepted A2A task receipt with a `#failed` evidence fragment only for the observed FAILED
outcome; delivery state `failed` or a task receipt alone is insufficient. The scan and locked
mutation both revalidate this qualification, even after create completion or dispatcher restart.
Submission admission takes the same task-tree authority lock before its row/delivery locks,
so a concurrent submission either commits first and disqualifies the failure or waits and is
denied after the drain. No timestamp comparison is used to infer which turn governs a task.

This rule deliberately covers only first-brief bootstrap failure. Any additional delivery,
including queued, recorded, uncertain or failed recovery work, prevents automatic draining.
Later-turn native failures, historical failures lacking the confirmed FAILED evidence marker,
gateway outages, exhausted not-accepted retries, preparation exceptions and control/configuration
refusals remain delivery diagnostics and leave the attempt active. Uncertain messages are never
replayed by this projection. Late failure evidence cannot revoke newer accepted/completed work,
reopen a terminal task or mutate a noncurrent attempt.

For a qualifying failure, the attempt's owner-readable `evidence_refs` retain the message ID
and sanitized diagnostic. The task page shows the existing reconciliation reason; diagnostic
detail is available through the linked native session or owner REST attempt history. Revocation,
attempt/task state and the `task:updated` event commit together, and repeat passes are idempotent.

Turn failure does not prove runtime termination or a clean no-start. The writer claim and
capacity stay held until the reconciler confirms native deletion and settles the failed attempt;
the task retains its blocked/reconciliation diagnostic. Unknown deletion retries with capacity
held, and no prompt is replayed or replacement writer started. Owner cancellation uses the
existing API/MCP drain/fence/settlement path and takes precedence while disposal is pending;
the task page's existing cancel eligibility remains unavailable.

## Admission and integration ports

All persistence mutations take the caller's asyncpg connection inside one transaction. Lock
order is global admission key, request key, parent task, branch claim. Trusted cap settings
(default 3 per parent, 6 globally) count held reservations including uncertain creates/drains.
Idempotency is checked before reservation, and transaction rollback removes all admission rows.

Capacity counts reservations rather than task status. Blocked, waiting and completed tasks
still count while their attempts hold capacity; their runtimes may remain writable or have
an uncertain stop. Confirmed fencing releases the slot regardless of the task's diagnostic
status. There is no age-based expiry of abandoned blocked tasks. The parent can cancel its
managed tasks through the existing `task_cancel` tool; `task_get` supplies the current
version and attempt for that request. A `409 parent_capacity` includes `limit`, `held_tasks`
(task ID, title, status/reason, version, current attempt, held attempt and attempt state),
and guidance to cancel abandoned work. The list covers only the caller's admission bucket,
including root tasks in the owner's main bucket. MCP returns these details in the error
text and owner REST returns them alongside `detail.reason`. Cancellation does not free a
slot until runtime termination is confirmed; uncertain disposal remains visible in the list.

Writer claims key owner/canonical repository/exact branch, covering project URL/name aliases.
Generations increase when a released claim is reused. Compare-and-swap release checks generation
and durable fenced state for delegated attempts. Coordination has no claim. S1 connects ordinary owner workspace creation to this helper, requires confirmed fence evidence
for owner release, and installs lifecycle guards. This is an offline-qualified implementation,
not live runtime fencing proof.
No retained sessions, topics or claims are backfilled.

Provisioning exposes `create`, `cancel`, `reconcile`; handoff exposes `start`, `reconcile`;
projection exposes `refresh`. Disconnected mutations return stable reasons and cannot dispatch.
Startup installs the existing publication projection port alongside provisioning at the same
installation seam. The existing task reconciler observes at most ten current active coding
tasks in a two-second share per pass, rotating by task ID with a process-local cursor. It
advances before each refresh and isolates revoked, terminal or failing sources so later tasks
can progress. A restart begins a new scan. The port revalidates source authority and only
records observations; qualification/activation gates and unavailable action flags are unchanged.
Later ports must persist intent in the caller transaction before external operations, use the
operation ID for reconciliation, consume the pinned attempt routing, and qualify readiness before
releasing a brief. They must not issue native calls while holding the admission transaction.
`update_projection` atomically updates observations/version and reserves an outbox event; it
cannot complete a task or grant consent. S4 owns trusted publication completion.

The managed MCP listener installs the same provisioning and projection ports after connecting
to PostgreSQL. It accepts transactional task intent and serves stored task reads alongside the
API process; only the API starts the task dispatcher and native reconciliation loop. Installing
ports does not create a runtime, send a turn or refresh a projection.

Creates already recorded as `blocked` / `provisioning_unavailable` remain terminal after ports
become available. The reconciler skips them, and an identical `delegate` or `POST /tasks` with
the same principal and `request_id` returns the original blocked operation. Changing that
request's payload still returns 409. Main or the owner must submit a new `request_id` to make a
fresh attempt. The blocked operation holds no task, attempt, capacity slot or branch claim.

## Notifications and recovery

```json
{
  "type": "task:updated",
  "event_id": "event-1",
  "task_id": "task-1",
  "version": 2,
  "attempt_id": "attempt-1",
  "root_task_id": "task-1",
  "parent_task_id": null,
  "occurred_at": "2026-10-07T00:00:00Z"
}
```

Task events are reserved in the mutation transaction. A separate connection publishes only
committed rows to the existing owner SSE bus, with SSE event name `task:updated` and the same
stable event ID. Delivery is at least once: duplicates and missed/disconnected events reconcile
through GET, never replay agent prompts. The dispatcher also routes persisted pending operations
to installed ports; blocked/unavailable operations never dispatch. No DBOS workflow sequence
changed in S0.

Retention settings reserve archive at 21 days and delete after two calendar months from
supersession. Validation conservatively requires archive no later than 28 days times the deletion
month count, guaranteeing order for any calendar. Destructive cleanup and holds are S3 behavior,
not implemented by S0. No transcript conversion, snapshot import or automatic fallback exists.

Task detail also includes `operations`, `artifacts` and `reports` arrays (empty before their
producer slices connect). Artifacts include operation ID, kind, SHA-256 and canonical JSON
payload. `unverified_provider_summary` remains a labelled claim; immutable manifests/checkpoints
are capped at 32 KiB and summaries at 8 KiB. Task operation records retain the validated
`request_payload` for durable recovery. Reports never imply completion or release capacity.
Report and recipient delivery tables provide stable request/event keys for S2 outbox routing.
Writer release requires a server-supplied `fence_evidence_ref` as well as generation/identity CAS;
S1 verifies that evidence, including ordinary owner workspace fencing, before calling it.

Action eligibility may express an available action without a blocker:

```json
{ "available": true, "reason": null }
```

An unavailable action must carry a typed reason; omitting it or passing null is invalid:

```json
{ "available": false, "reason": "handoff_unavailable" }
```

For cancel/retry/reassign, mutation locks are acquired in global admission, request key,
target task order. The target is authorized before any operation FK is inserted; missing or
foreign-owner targets return the same scoped 404 `task_not_found`. An identical authorized
request returns its existing operation before checking task version, preserving lost-response
replay after a version change. A changed payload still returns 409.

## Operator smoke observations

`GET /projects/{project_id}/smoke-observations?branch={branch}` is an owner-only,
read-only ledger snapshot. Missing and foreign projects return 404 before any
ledger reads. It does not sync native sessions, dispatch work, reconcile state,
or expose grants, credentials, messages or raw session data.

The response contains:

- `parent_capacity` and `capacity_holders`: configured owner-main admission limit
  and owner root task IDs whose attempts still hold capacity, across projects.
- `global_capacity_available`: a boolean only; other owners' identities are hidden.
- `deliveries_busy`: true when the owner's project or main session has any
  recorded, sending, delivered, queued or uncertain delivery. Uncertain delivery
  is a hold until separately reconciled; this route does not change owner rules.
- `deliveries`: at most 100 objects with `message_id` and `state`, ordered by ID.
  `deliveries_truncated` indicates omitted rows. Busy remains true if truncated.
- `pushes`: at most 100 objects with `request_id`, `state` and `branch`, for the
  exact supplied branch and owner/project, ordered by newest update then ID.
  `pushes_truncated` indicates omitted rows. Without a branch this list is empty.
- `push_confirmed`: an independent existence check for a confirmed publication
  on that branch; omitted detail rows cannot hide confirmed evidence.

Detail reads fetch at most 101 rows to detect truncation. This snapshot confers
no execution authority; task admission and publication policy remain authoritative.

## React status (F6)

Labels: **implemented** means built in `web/` and covered by component tests against MSW. Nothing here is live-proven.

- **Implemented:** the task page at `/tasks/$id`. It shows the status, title, brief and parent link, then these sections in order: `[PROVIDER]`, `[WORKSPACE]`, `[PR · CI · MERGE]`, `[IN PROGRESS]`, `[ACTIONS]`, `[ATTEMPTS]`, `[REPORTS]` and `[CHILD TASKS]`. A pending owner action links to the Inbox. A provider summary is labelled as unverified.
- **Implemented:** owner actions are Retry, Reassign and Cancel task. Each is enabled only when the server reports it available, nothing is in flight, the connection is up, and no unconfirmed request is held. Every submission carries the version and attempt the owner saw. Reassign offers only the enabled other providers that offer the role; the server still decides whether the target is accepted. Cancel asks in an in-app dialog; **Keep running** sends nothing. If the task changes while that dialog is open, the dialog closes and sends nothing, and one line says so. That line stays until the owner acts again; a new **Cancel task** asks again, on the version now shown.
- **Implemented:** a lost reply (no response, or a 5xx) is kept as **Resend original request** with its original version, attempt, target and request ID. **Discard** drops it. A 409 or 404 re-reads the task before the notice appears. The notice shows the backend's message, and the reason code beneath it as `Server reason: <code>`.
- **Implemented:** an unconfirmed request is kept only while its task stays on screen. Leaving the page drops it, so a later visit offers no **Resend**. A resend carries the original expected version and request ID, which the server checks. Whether that stops a doubled change when the first request was recorded is not proven against a live backend.
- **Implemented:** version-gated live updates. An older task read never replaces a newer one held. A `task:updated` event refetches the task only when its version is newer than the held one. The task list takes its membership from the latest read. A failed refresh keeps the last view under "Showing the last known state" with **Retry loading**. Only a successful task read clears that line.
- **Implemented:** the project task tree (`TaskTree`) and the tasks count (`TasksBadge`), both exported from `web/src/features/tasks`. A row shows CI only when the PR head, the CI head and the observation agree and are fresh. Otherwise it shows `CI unknown`. The compact count caps at `99+`, as the inbox badge does. A row this page cannot read is not shown, and the list says how many: "1 task could not be read and is not shown."
- **Wording:** while the connection is down, every action is disabled with "Mainloop is offline." A resend waits for the connection probe to confirm the link, so a lost reply on a dead link cannot be resent blindly.
- **Wording:** the task list is read under its own query key, below the `keys.tasks()` prefix. It does not share the sessions list's `['get', '/tasks']` query. Live invalidation still reaches it. A task page reads its parent by id and its children through the list's `parent_task_id` filter, so it does not read the whole list.
- **Implemented:** the tasks count is mounted on the compact **Inbox** tab, and its words are part of that tab's accessible name. The tree is not mounted in the shell: the project route that would show it is not part of this slice.
- **Implemented:** the compact task count has a shared visible-only poll fallback for missed live events, using the same task-list cache as the tree and badge. Task detail and list reads consume cancellation signals, so leaving their last observer or hiding an active poll cancels its read.
- **Not proven:** any run against a live backend, the live SSE stream (the event handling is tested through the applier, not a real stream), and layout at phone width in a browser.

## Go status (G8a)

Labels: **implemented** means built in `server/` and covered by tests on a scratch PostgreSQL. Nothing here is live-proven.

- **Implemented:** the task record (migration `00060_tasks`): tasks, attempts, operations, artifacts, reports and event deliveries. The database refuses a changed owner, mode or hierarchy, a version that goes down, a second unfenced attempt for a task, and a writer generation that does not rise by exactly one.
- **Implemented:** the task, attempt and operation state machines in `server/internal/tasks`. Every allowed and refused move is tested.
- **Implemented:** `GET /tasks`, `GET /tasks/{id}`, `GET /task-operations/{id}` and `POST /tasks`, `/tasks/{id}/retry`, `/reassign` and `/cancel`. Reads carry the task version. Writes answer 202 with an operation. An operation is keyed by owner, principal and request ID. The same request ID with the same payload returns the stored operation; with a different payload it answers 409 `request_payload_conflict`.
- **Implemented:** the MCP tools `delegate`, `report`, `task_get`, `task_history` (the same read as `task_get`), `task_list` and `task_cancel`, with the per-actor visibility in `api/mcp-tools.json`. A supervisor or child binding authenticates only while its attempt is the task's current, live attempt. `whoami` returns the task fields. Only an `active` attempt authenticates; `creating` does not. A child also needs its parent's current attempt to be `active` and the parent task to be open. Every write re-checks the caller's attempt under the task lock, so an attempt drained after authentication is refused with 403 `inactive_principal`. Merge status stays readable only when merge tools are on, the grant is `workspace`, and the attempt is still `active` and current while its task is `completed`; G10 adds the condition that the task has a merged proposal. `owner_action` is accepted only with `blocked`; a later blocked report with a new `owner_action` replaces the earlier decision even when the status does not change. Agent reads blank the owner-only fields: operation `principal_key`, `request_payload`, `request_digest`, and attempt `evidence_refs`, `result_ref`, `brief_delivery_id`, `retention_hold`.
- **Implemented:** every task change commits with its `task:updated` event in one transaction, from the API and the MCP process alike. A rolled-back change publishes nothing.
- **Implemented:** the preview lookup refuses a workspace owned by a task attempt unless that attempt is `active` and the task's current one.
- **Implemented (G8b):** `delegate` and `POST /tasks` record the task and a `create` operation, and the reconcile loops below provision it. Cancel of a task with no attempt completes at once and ends the task's `requested` create operation (`blocked`, no reason), so a cancelled task is never provisioned. Cancel of a task with an attempt moves the operation to `draining`; the attempt is stopped, fenced and ended.
- **Blocked by contract:** retry and reassign return a `blocked` operation with reason `handoff_unavailable`, and the task view reports both actions as unavailable. The F6 page shows what the server reports, so Reassign stays disabled.
- **Not built:** the publication projection beyond the GitHub observation (G8b), and the Python behaviors listed under G8b as not built.

## Go status (G8b)

Labels: **implemented** means built in `server/` and covered by tests on a scratch PostgreSQL with fakes. The tests use a fake workspace runtime, the real sessions service with no kagent client, and fake GitHub observations or the production observer against `githubtest`. Nothing here is live-proven.

- **Implemented:** provisioning (`task-operations` loop). A `requested` create operation locks the task, re-checks it is open, and inserts the attempt (`creating`, capacity held, writer claim for a code task) in one transaction. A coordination task then gets its session and binding from the sessions service. A code attempt also stores an immutable `workspace_request_id`, derived from its attempt ID, before calling the workspace service's idempotent `CreateWithRequestID`. A replay looks up only that request ID before retrying, so an owner's workspace on the same project and branch is never adopted, briefed or deleted by the attempt. The attempt waits for its workspace's kagent Session, then goes `creating → active`, the task `running`, and the brief reaches the agent as one ledger message with a derived id, written before the attempt notes it, so a replay finds it. An interrupted create never makes a second workspace, session or brief.
- **Implemented:** workspace cleanup for an attempt that never received its brief. Cancel and abort delete only the workspace with the attempt's request ID. Each workspace service call has a durable reservation (`workspace_creates_inflight`), committed under the task lock before the call and released when it returns, without a transaction across the call. A drain prevents new reservations; a stale operation claim cannot reserve a call. The settle sweep also checks final attempts until `workspace_cleanup_confirmed`: the workspace is absent and every reserved call has returned. Ending the create operation alone is not proof of absence. A workspace that commits after cancel completed is deleted on return when attachment is refused, or by the sweep if the return was lost or deletion was unconfirmed. An interrupted process that never records its call's return leaves cleanup unresolved; the sweep continues checking its request ID without assuming that a timeout proves absence. Attempts that received a brief retain their workspace for inspection.
- **Implemented:** unresolved final workspace cleanup uses a separate batch of at most 50 attempts per settle sweep, so it cannot consume the runtime stop resolver's batch. Each unresolved look, including an observation or deletion error, durably advances `workspace_cleanup_after` with exponential backoff from 5 seconds to a 5-minute cap. Unchecked cleanup precedes deferred cleanup; due retries are ordered by their retry time. A returning create clears the retry time for a prompt fresh look. Retry time never releases an uncertain reservation or confirms absence. Owner cancel of an already-final task records a completed no-op and preserves its outcome; the settle sweep still finishes runtime disposal and pending workspace cleanup independently.
- **Implemented:** delegate brief prefix. The agent's first message opens with its role and tool guidance (`whoami`, `task_get`, the pull request tools for code, `report`) and then `## Assigned work` with the brief as written. Code tasks add the workspace-environment notes. The stored brief is unchanged.
- **Implemented:** writer claims. A code task claims `(owner, repository, branch)`. The generation rises by exactly one when a released claim is taken again. A branch that another attempt holds leaves the create `requested` with no attempt until the holder is fenced or the provision window passes.
- **Implemented:** capacity. `TASK_MAX_CHILDREN_PER_PARENT` (3) and `TASK_MAX_CHILDREN_GLOBAL` (6) count child tasks that hold a slot: an attempt with `capacity_held`, or a task with no attempt yet. A refusal is 409 `child_capacity` or `global_capacity`, decided under an owner-wide lock. A slot is freed only when the attempt is fenced with the runtime stop confirmed. The stop is a durable state on the attempt, `stop_state`: `none` while it runs, `requested` from the moment it moves to `draining` (written in the same transaction, before any kagent call, so a crash leaves the request), `confirmed` once the stop observer has proven the session stopped, and `unresolved` after the bound. The database refuses `creating`/`active` with anything but `none`, `draining` with `none`, and `fenced` without `confirmed`, so a claim and slot cannot be released on a guess. Only the observer confirms: the result of cancelling the session in the sessions ledger only closes Mainloop's own open deliveries, and `evidence_refs` is a diagnostic record that nothing reads for safety. The observer takes every kagent context the session has used (the binding's, and each delivery's own), lists all the tasks in each, and cancels any that is not final; a read error, a task it cannot read, or a task it had to cancel means "not stopped yet". A message that was sent but that no kagent task carries yet counts as possibly running until the sessions ledger's hold (`SESSION_UNCERTAIN_HOLD_SECONDS`, 1800) has passed since its row last changed, so an owner cancel that races an in-flight send keeps the claim and slot until the landed turn is stopped; a message that was never sent cannot be running. A resolver (in the settle sweep, or in the cancel operation that is waiting) asks the observer with backoff. After 6 looks without a confirmation the attempt becomes `unresolved` and its task is blocked (`blocked` / `reconciliation`) if still open, and the resolver keeps looking at the slowest pace; a later confirmation still ends the task `cancelled` and releases the claim and slot. A task that already ended (a child cancelled with its parent) is not blocked: it keeps its claim and slot until the stop is confirmed.
- **Implemented:** abort. A refused workspace, a workspace session that does not start within `TASK_PROVISION_WINDOW_SECONDS` (900), or a branch that stays held takes the attempt `creating → draining → fenced → failed`, fails the task, and blocks the create operation (`provisioning_unavailable`). A cancel that arrives mid-provision removes a workspace the agent never used and ends the task `cancelled`, not `failed`; an abort that finds the attempt already taken by a cancel blocks its create operation and leaves the task to the cancel. A workspace request that can never succeed (invalid, or a project that does not exist) aborts at once instead of retrying; any other create failure (database, transport) is retried with backoff only within the provision window, then aborts.
- **Implemented:** settlement. Drain, stop the session, revoke credentials, release the claim, fence, final. Every step is idempotent on the attempt's stored state. Lock order is the task, then its child tasks in id order, then the attempt; a supervisor's drain cancels its open children in the same transaction (task, child tasks in id order, attempt), so they stop even while the supervisor's own stop is unconfirmed; their attempts then settle on their own. A child's report and every child write also hold the parent's current attempt `FOR SHARE` and refuse when it is not `active` or the parent task is closed. A sweep (`task-settle`) finishes attempts left behind by a crash, and ends the attempts of completed coordination tasks. The attempt of a code task that reported `completed` stays live for merge status.
- **Implemented:** reports to parents (`task-event-deliveries` loop). Each report row sends its text (`[report from child task <id> attempt <n>; <outcome>; unverified claim]`, plus `Owner action needed:` when set) to `parent-task:<id>` and, for `blocked` and `failed`, to the main thread, through the sessions ledger under a derived message id. A busy session (one open turn) is retried later. A parent that ended after the report committed gets no message: the delivery is `cancelled` and the report stays on the child. A ledger row that failed is sent again under a new id, three sends in all (a parent that has ended meanwhile gets no message and the delivery is `cancelled`). A row that ended `cancelled` is the owner stopping the parent's turn, or the parent's session ending: the report is not sent over it, the delivery ends `cancelled`, and the child is not blocked. After three failed sends the delivery is `uncertain`, `native-delivery-failed:<id>: <detail>` is written to the reporting attempt, and its task becomes `blocked` / `reconciliation`. The first brief is sent and watched the same way: a send to a session that has ended, been archived or is missing gives up at once (`brief-delivery-failed` evidence and `blocked` / `reconciliation`), while a busy session or one whose workspace is not ready is tried again. In the `task-briefs` sweep, a ledger row that failed is sent again under the next derived id, three sends in all, then the attempt gets `brief-delivery-failed` evidence and its task becomes `blocked` / `reconciliation`. A session that has ended or been archived gives up at once the same way; a busy session, or one whose workspace is not ready, is tried again on the next pass. A row that ended `cancelled` is the owner stopping the turn and steering the child: the attempt is left as it is and the brief is not sent again.
- **Implemented:** GitHub observation (`task-projection` sweep, every `TASK_PROJECTION_SECONDS`, 60). For open code tasks with an attempt it reads the pull request and CI of the task's branch through an `Observer`, stores them on the task and publishes `task:updated`. CI in any state counts only when its head equals the pull request head; otherwise it is `unknown`. A pull request for another branch or repository is ignored, and a PR identity read error makes the projection unavailable. A CI-only error keeps the freshly rechecked PR and description with CI `unknown` and no CI head SHA; partial CI pages are discarded. A moved or missing PR, cancellation or stale observation is not kept. An unchanged observation only refreshes `projected_at` (no version, no event). A projection never changes a task's status.
- **Implemented (G10b):** the production GitHub observer is installed in `serve-api` when the GitHub App is configured. It lists all PR states for the task branch, prefers the newest open PR and otherwise the newest closed or merged PR, and validates both repositories and the branch. That pick applies only when the task has no recorded PR (see the next item). It reads all check-run and commit-status pages for the exact PR head, uses the latest run of each `(app, name)` and latest status of each context, then re-reads the PR. A changed head or identity is not observed. No results mean `unknown`; a failure outranks pending, and only successful results from every reported check/context give `success`. Skipped, neutral and unfamiliar results fail closed. This is CI observation, not merge-policy readiness: required checks, rules and check suites are evaluated by the merge service.
- **Implemented (PRLINK):** a task links the pull request its current attempt opened through Mainloop. The sweep reads the newest `created` `pr_creations` row whose session id is the attempt's binding id (the binding is unique to the attempt) and whose owner, project and head branch are the task's, and passes its PR number to the observer as the expected PR. The observer then reads only that PR and reports it open, closed or merged; if it is missing, or is not a PR for the task's branch and repository, it reports no PR and does not pick another from the branch, so a second PR on the branch, or one someone else opened, never replaces it. A new attempt has a new binding, so it does not inherit the previous attempt's PR, as on main. With no such row (the agent opened the PR outside Mainloop, or has not opened one) the branch pick above applies.
- **Implemented (G10b):** observations are read afresh on every sweep, with a five-minute collection bound. The observer captures `observed_at` when collection finishes, before database work, and persistence preserves it in `projected_at` for both changed and unchanged facts. A database delay never refreshes old evidence. Task reads use only the database and mask CI to `unknown` when `projected_at` is absent, in the future or older than five minutes, or the CI head does not equal the PR head. Job completion time is independent of observation freshness. Rate limits (including rate-limit 403s) and 5xx/transport failures are classified for a later sweep; other 4xx refusals fail closed. A 404 means absent: no PR when its read is missing, or a known PR with `unknown` CI when a CI source is missing. The observer makes no repository writes and requests read permissions only.
- **Implemented:** retry and reassign return the `blocked` operation (`handoff_unavailable`, source attempt set) on every path after the target provider is validated; a task that is already final refuses with 409 `task_terminal`.
- **Implemented:** agent views. An agent sees the reports of its task's current attempt only, no artifacts, no checkpoint or manifest refs, and no `pending_approval_ids`. Python also showed continuation reports and the continuation artifact pair for a linked continuation; no continuation exists until handoff is built.
- **Not built:** handoff (retry and reassign) and the publication projection that decides a task's merge state. Without a configured GitHub App, the observation sweep does nothing.
- **Not proven:** any run against kagent, a real workspace, GitHub or a cluster.
