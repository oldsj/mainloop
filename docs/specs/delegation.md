# Task delegation and reports

Mainloop's MCP tools create and read durable tasks through the same application service as owner task requests. They do not call owner REST or grant owner consent. PostgreSQL owns task, attempt, hierarchy, capacity and writer authority; native agent activity remains separate.

## Tool scope

Main at depth zero creates supervisors. A supervisor at depth one creates direct children within its persisted project and task tree. Children at depth two and ordinary owner workspace agents cannot delegate. Discovery and direct invocation use the same exact role, depth and grant rules; each task call revalidates persisted identity and current ancestry. Missing, superseded, revoked, archived or terminal attempts cannot authenticate.

`delegate` accepts `request_id`, `title`, `brief`, `mode`, optional `project_id`, `topic_id`, `provider_profile_id` and typed `checkout`. Code tasks require an owned project and checkout; coordination tasks have no repository authority. The service chooses role, depth, owner, root and native configuration. Provider constraints and admission caps apply through the shared service. Reusing a request ID with identical input returns the original operation; changed input conflicts. A returned pending operation is not a running native session.

Reuse the request ID after a lost or uncertain response. After a confirmed terminal `blocked` create, a new create requires a **new** request ID; replaying the old ID returns the recorded blocked operation.

Main's standing context lists its owner's projects with IDs, full repository names and whether an environment is selected. Selection does not guarantee environment readiness. This index is a snapshot when standing context is installed, not a live project query, and is not included for delegated agents.

MCP-created task briefs include short role guidance before the assigned work: call `whoami`, read `task_get`, then perform the work and `report`. Coding guidance additionally says to run project checks, `git push` the task branch, use `open_pull_request` instead of `gh` (the workspace cannot reach GitHub's API), and use `merge_pull_request` once CI is green, subject to existing policy and tool availability. If it returns `approval_required`, call `merge_pull_request_with_approval` (the owner approves in Mainloop), or report missing approval tooling as a blocker. Coordination guidance grants no repository authority. The combined guidance and assigned brief must fit the existing 16 KiB brief limit. Owner-created task briefs are unchanged.

`task_get`, `task_list` and `task_history` read stored task projections, attempts, operations and reports without creating native turns. Main reads its owner's trees; supervisors read themselves and their direct children; children read only themselves. `task_cancel` requires the current task version and attempt and follows the existing durable cancellation service. Supervisors manage only their direct children, not themselves or another tree.

`task_retry` and `task_reassign` remain hidden and directly denied until the shared handoff port is installed. No provider fallback is inferred. Legacy session tools `status`, `read`, `cancel` and `clear`, and topic/kind delegation arguments, are removed.

`whoami` returns server-resolved task, attempt, parent, root, role, depth, workspace, attempt number and writer generation alongside existing scoped identity. A workspace grant supplies repository tools only within its own checkout; coordination grants supply no repository or merge tools. Existing PR/merge consent and enablement gates remain in force.

## Reporting and completion

A supervisor or child reports only its own current attempt using `report(task_id, attempt_id, request_id, summary, outcome, evidence_refs)`. Outcomes are `progress`, `completed`, `failed` or `blocked`. Each logical report has a stable request ID: equal replay returns its recorded result, changed content conflicts, and additional progress uses a new ID. Reports are untrusted claims, not instructions or owner approvals.

The report, task update, task event and recipient intents commit together. Child reports route to the current authorized parent attempt and roll up to main. Root reports route to the current authorized main session. Owner task reads and committed task events expose the same reports and blocked/waiting state; this does not fabricate owner approval cards or transfer native HITL identity.

Notifications use deterministic message IDs and the existing native delivery queue, including busy or owner-held parents. Restarting the existing task reconciler recovers committed intents without another MCP call. A revoked recipient retains a pending intent; a queued report for a superseded or revoked recipient is cancelled before rerouting to current authority. Sent or uncertain deliveries retain their original identity and are never replayed to a successor. A report does not choose its recipient or escape its tree.

A completed native turn or fallback reply creates no report and completes no task. Coding `completed` reports leave the task waiting for publication; S4's verified merged publication is required for coding success. `failed` and `blocked` reports preserve authority and capacity pending reconciliation rather than claiming the runtime has stopped.

Coordination completion requires an explicit completed result and no live child reservations. The reconciler waits for outstanding native deliveries, drains the attempt, revokes its credential, confirms runtime deletion, then settles completion and releases capacity. Unknown deletion keeps the attempt draining and the result pending. A pending cancellation takes precedence over completion.

The same cleanup runs for coding tasks after verified merged settlement. Product completion
remains visible while runtime deletion is pending; it does not free capacity by itself. The
reconciler discovers older completed tasks with held active/draining attempts without a new
operation or manual database repair. Failed/cancelled coding outcomes with held attempts are
also recovered; ordinary cancellation and provisioning failure already use confirmed settlement.
An authoritative failed first/sole brief already draining is disposed through this path, retaining
the task's blocked/reconciliation diagnostic. Active failed/blocked reports, publication holds,
and handoffs remain reservations pending their existing resolution paths. Live children,
outstanding or uncertain deliveries, unresolved publication and pending operations prevent
automatic release.

Standing context renders bounded stored task projections and recent report claims without additional model calls. Native history, compaction and structured owner input retain their provider identity. The delegated runtime receives the persisted initial brief, including role guidance for MCP creates; the richer standing projection is not injected into that brief.

## Qualification boundary

Offline tests use real isolated PostgreSQL and fake kagent responses. They establish persistence, authority, queueing and restart behavior. They do not qualify live checkout composition, provider delivery, gateway isolation, scoped publication or production enablement.
