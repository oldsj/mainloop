# Durable tasks

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

An omitted checkout `depth` is 0, which leaves clone depth to the runtime default; a positive
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

Task GET authorizes the task before reading its cached publication projection. Reads stay
DB-only: successful CI becomes unknown when its head differs, its timestamp is missing,
future or older than five minutes. The response includes the existing publication mode and
current merge proposal/result; reading never starts a native turn or dispatches a merge.

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
`cancelled`, `completed`. Capacity remains held separately until confirmed fencing/termination;
reporting alone never releases it. Unknown creates/stops/publication retain their reservation.

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
