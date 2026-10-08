# Merge policy and HITL storage contracts

Implemented foundation: project policy API, immutable protected-path rules, typed HITL
payloads, observed-session/checkpoint storage, verified alias storage, and durable owner
decision receipts, plus background inventory discovery and task-bound owner continuation
described below. Disabled merge execution is described in [Pull requests](pull-requests.md#merge-tools-implemented-disabled-by-default); shared inbox/chat controls remain a separate slice.
Production enablement still requires the isolation and native capability evidence below.

A newly enrolled owner workspace may discover and invoke merge tools only when the existing
`MAINLOOP_MERGE_TOOLS_ENABLED=true` gate is enabled, and its PR identity must satisfy the same
server-resolved project/repository/branch scope used by PR creation. Workspace enrollment does
not change merge policy, mint HITL consent, enable production flags or grant owner REST access.
The normal production manifests keep merge tools disabled.

## Owner policy

New projects and existing projects migrated from the schema without policy columns default
to `auto`, version 1, including Mainloop and testrepo. Startup migration is idempotent;
repository refresh and project upsert preserve owner choices. There is no repository seed.

`GET /projects/{id}/merge-policy` returns `merge_policy`, `merge_policy_version`,
`protected_globs`, and `protected_globs_version`.
Policy mutation is **disabled by default**, returning 503 without updating policy or audit.
An operator may set `MAINLOOP_OWNER_POLICY_WRITES_ENABLED=true` only after establishing
owner-route isolation for both direct and external-gateway access. Only the exact value
`true` enables writes; no development-mode bypass exists. This is a deployment opt-in,
not requester authentication: once enabled the existing network boundary must exclude
actors whether they omit Authorization or present an MCP bearer. No manifest enables it here.

`PUT /projects/{id}/merge-policy` accepts only `merge_policy: auto|approval` and
`expected_version`. A changed version returns 409; a missing or other owner's project
returns 404. Actual changes increment the version and append an immutable audit row. Saving
the same value at the current version is a no-op. Updates lock the project row; future merge
claims must take that same lock before reading current policy.

Protected globs are server constants, version 1: `k8s/**`, `.github/**`, and
`**/migrations/**`. Matching is case-sensitive and repository-relative POSIX; `**` includes
zero directories. Deleted paths and both sides of a rename count. Missing rename sources,
invalid or incomplete paths, and more than 3,000 files fail closed. An approval policy or a
protected match requires approval; callers cannot edit the globs or select an execution mode.
This helper establishes path policy only; the merge service must prove GitHub path
completeness and all other merge gates.

The owner API uses the existing configured-owner/network-isolation boundary. It does not
add request authentication. **Production enablement requires proof that actors cannot reach
owner routes directly or through an external gateway, with or without an MCP bearer.** Unit
and isolated PostgreSQL tests do not prove deployment isolation.

## Correlation and storage

`models.hitl` describes the versioned kagent extension's approval and ask-user requests and
responses. Native free-text questions preserve `choices: null`; list bounds and malformed-type
validation still apply. Approvals cannot contain a nonempty rejection reason; actual denials
retain their reason. Builder and storage validation reject contradictions before reserving
a response key or recording consent. Unknown metadata survives decoding but confers no authority. Payloads are limited
to 128 KiB; batches/questions have at most 100 members. Malformed/unknown requests cannot
produce an actionable receipt. The gateway decoder retains metadata/extensions; the observer validates these types.

The outer continuation retains endpoint, gateway, runtime session, context, task, status
message, request hash, and the complete bounded request (including parent IDs). The leaf
retains independently verified task/session/context, binding when available, pending ID,
native call ID, tool name, arguments hash, and request hash. A Mainloop child observed directly
has a direct route; its parent session alone does not establish propagated A2A delegation.

Only server-verified control-plane creation or gateway continuation associations allow a
propagated leaf. Hints, child IDs, subagent names and arbitrary metadata cannot establish one.
An unverified nested claim remains unavailable with no leaf aliases; it cannot reserve or
disable a verified direct request. Standalone questions require verified session ownership,
but do not require an MCP binding or merge configuration.

`native_observed_sessions` stores verified owner/creator/agent/endpoint/context/revision
snapshots. Observer cursors are stored independently of native deliveries. Save observation
upserts and the covered checkpoint in the same transaction. A changed owner is rejected.
`native_hitl_requests` and `native_hitl_aliases` are rebuildable. Projection and inbox writes
share a transaction, ensuring the main-thread FK row exists and one inbox reference per
projection. Read/unread is presentation state. Consumers must derive pending/answered/
unavailable from projections and receipts, not queue response/status fields. Before enabling
these cards, the continuation slice must reject the legacy queue response/dismiss routes.

The response uniqueness key is SHA-256 of the canonical JSON array
`[gateway, leaf_runtime_session_id, leaf_task_id, pending_request_id, leaf_request_hash]`.
Reused native call IDs in different tasks cannot collide. Lock verified leaf keys in sorted
order before alias route selection and response recording. The observer/response consumer
must choose the outermost verified route before first recording and revalidate associations.
Every batch member reserves its leaf key atomically. Unverified aliases never join these locks.

`native_hitl_responses` and its member rows are immutable, with no foreign key to projections.
Same owner/action ID and complete body return the original receipt; a changed body conflicts.
Any already-answered leaf conflicts under another action or alias. Its original outer
destination/message ID cannot change when new aliases arrive. Rebuilding/deleting projections
cannot erase consent or uncertainty. Transport state is separate (`recorded`, `sending`,
`accepted`, `uncertain`, `rejected_transport`); the b2 task-bound lane dispatches and reconciles these states.

## Frozen merge receipt contract

`MAINLOOP_MERGE_CONFIGURATIONS` is a bounded operator-owned JSON array keyed by
`template_name` and `provider`. Each entry contains the configured compiled alias, endpoint,
the `merge_pull_request_with_approval` tool, `require_approval: true`, and canonical operation
`mainloop.merge_pull_request_with_approval.v1`. It contains no Mainloop binding, kagent Session,
or prepared-revision IDs. The base ConfigMap keeps the merge gate false and this array empty.

When resolving a pending owner decision, Mainloop reads the bound kagent Session through its
trusted client, verifies its configured creator, Session identity, Agent reference, and live
Mainloop binding, then reads that Agent and uses only its named `templateRef`. Inline-template
Agents and unknown or ambiguous `(template, provider)` entries receive no merge authority.
`canonical_operation(public_name, configuration)` requires the exact provider-qualified public
tool name derived from the configured alias, along with the configured endpoint, tool, approval
flag, and canonical operation. Claude uses `mcp__<alias>__merge_pull_request_with_approval`;
Codex uses `<alias>.merge_pull_request_with_approval`.

Each proposal presentation snapshot and summary digest includes a server-computed mapping
reference containing template name, provider, and a digest of the matched config entry. Mainloop
re-reads the Session, Agent, and config when the owner responds. A changed or missing mapping
reference makes positive approval unavailable; rejection remains available. Pending requests
using the retired per-session config are not migrated and must be reissued. Existing immutable
receipts remain readable for reconciliation.

This simple mapping does not pin AgentTemplate contents or RemoteMCPServer identity. A reviewed
template edit can point the configured alias at a different server without changing this
reference. GitOps review governs template contents; the protected server's route filtering and
the recorded-consent gate still apply. This is a residual risk of the simple variant.

For the verified alias `mainloop-merge-approval`, the only valid public names are:

- Claude: `mcp__mainloop-merge-approval__merge_pull_request_with_approval`
- Codex: `mainloop-merge-approval.merge_pull_request_with_approval`

Other aliases require explicit template/provider entries. Bare names, suffix matches, another
server's same tool, and other tools with identical proposal arguments remain generic. A suffix
may show an unavailable merge notice and block positive response when mapping evidence is
missing, but it never establishes the canonical operation.
The approved merge arguments contain exactly `proposal_id` and `request_id`. Hashing uses
UTF-8 JSON, sorted keys, compact separators, unescaped Unicode, no NaN, then SHA-256.

`MergeReceiptKey` contains `owner_id`, `leaf_binding_id`, `leaf_runtime_session_id`,
`operation` (the literal canonical operation), `proposal_id`, and `invocation_request_id`.
`lookup_merge_receipt(conn: asyncpg.Connection, key: MergeReceiptKey,
approved_arguments_hash: str) -> DecisionReceipt | None` matches that entire key and exact
arguments hash, requires a positive decision and rejects ambiguous matches. It validates the
retained leaf/outer/request/call/configuration snapshot; it never joins a mutable projection.
A sibling, parent, replacement runtime, changed invocation ID or altered payload cannot use it.

The handler must get binding/runtime identity from live MCP authentication and operation
identity from its own protected registry. The merge proposal resolver validates owner, binding and immutable proposal facts when building a receipt. Its callback contract is
`Callable[[MergeReceiptKey, bool], None]`, where the boolean is the per-call approval;
it raises on failure, allows rejection of stale owned proposals, and refuses stale approval.
The merge service must claim the positive receipt with one persisted intent, serialize with
policy/rejection gates, and recheck all current authority, candidate and GitHub evidence.

Recorded consent does **not** attest that a later raw invocation traversed native approval.
Lookup intentionally does not wait for transport acceptance, so the resumed tool cannot
deadlock on its own continuation. A raw call after consent can only be allowed for the exact
operation, once, by the merge-intent gate. No live native restoration or provider
capability parity is claimed by the fixtures.

## Background discovery and structured continuation

Implemented b2: backend startup starts the existing reconciliation loop, whose independently
protected HITL observation step discovers SessionService inventory even with no browser,
Mainloop binding, or delivery row. It never creates or resumes an Actor. The configured gateway
creator must match the server's configured kagent user and Mainloop owner before task contents
are fetched. Bound sessions must also retain their stored owner/runtime identity. Gateway
agent resource names select a relative endpoint under the configured gateway; runtime
`a2a_authority` is checked as a logical actor identity and is never fetched as a URL.
Missing creator, agent, revision, conflicting bindings, archived sessions, or replacements
cannot enable answers. A standalone observation creates no project, MCP credential, or delivery.

Inventory sweeps repeat every 30 seconds, continuing unfinished pages on subsequent passes.
Each pass processes at most 50 inventory records and 50 listed tasks, at most 10 task snapshot
reads including nested reads, with a two-second scheduling budget and per-operation timeouts.
Inventory and task enumeration each get at most half a second so failed listings cannot starve
known pending requests. PostgreSQL stores the inventory cursor and per-session task cursors;
least-recently-scanned sessions and least-recently-checked tasks rotate across restart. Upserts
and the cursor covering them commit together. Invalid tokens restart that enumeration without
inferring deletions. Partial listings and temporary failures never imply completion.

`GET /hitl-observer/status` exposes pending task count, oldest check time, and bounded inventory
diagnostics. This backlog describes observation work, not a guaranteed discovery latency.
Known requests become stale on transient reads and unavailable on confirmed deletion,
cancellation, archive, replacement, or changed pending identity. Valid new requests get a new
reference; repeated identical events retain the same reference/card. A changed status-message
ID with unchanged pending payload supersedes the earlier observation: its card expires and
its aliases leave route selection, while its snapshot and any recorded receipt remain.
Later failures cannot reactivate superseded controls. A genuinely unavailable verified parent
still holds its relationship; it is not treated as a superseded observation. Unsupported extension
payloads and `auth_required` produce non-actionable attention. Metadata and extensions survive
A2A decoding and stream projection, including changed payloads with an unchanged task state.

`GET /hitl/{request_id}` returns the owner-scoped projection, selected route, retained receipt,
transport state, and whether that particular view may answer. Inbox entries carry the same
`hitl_request_id`; read/unread remains presentation state. Card titles/status derive from
observations and durable receipts, including uncertainty after a rebuild. Generic queue
responses and internal generic status/response mutations reject HITL cards.

`POST /hitl/{request_id}/respond` accepts `action_id` and a complete typed `response`. This route
is disabled unless **`MAINLOOP_OWNER_HITL_WRITES_ENABLED=true`** exactly. Like the policy gate,
this is an operator opt-in after proving owner-route isolation, not request authentication.
No manifest enables it. Fresh ownership/task checks, exact batch validation, sorted verified
leaf locks and route revalidation precede immutable recording. Same action/body is idempotent;
another action or contradictory batch cannot take any already-recorded leaf member.

Trusted associations are read only from the server-owned association store. No public endpoint
imports associations and no payload field creates them. Deployment integration must populate
that store from verified creation/continuation evidence before propagated answers work.
Direct Mainloop children remain direct despite their parent binding. Unverified propagated
claims get separate unavailable cards with no leaf locks or aliases. Verified but unresolved
or ambiguous parents hold only their verified relationship. Before consent, the outermost
verified route is the sole answerable view; after consent, the retained receipt always wins,
including when new aliases arrive or projections are rebuilt. Nested questions preserve distinct
outer and child request IDs. Provider-local subagents have no assumed supported mapping.

Background response recovery has its own two-second budget per reconciliation pass, including
all remote phases across at most ten receipts. Attempt timestamps advance before remote work,
so interrupted receipts rotate behind untouched work across passes and restarts. Budget
cancellation after a send claim preserves `sending`; subsequent recovery observes uncertainty
without replay. Ordinary reconciliation and housekeeping proceed after that bounded share.

The task-bound continuation lane bypasses ordinary queued turns and never calls session
replacement or prompt delivery. The decision and outbound message ID are durable before a
send attempt is claimed. A crash while still `recorded` can recover and send; persisted
`sending` after a crash is uncertain and is observed without replay. Only exact outbound
message identity and structured response in the original task history prove acceptance;
a changed status alone does not. The client only automatically retries the documented definite
`KAGENT_SEND_NOT_ACCEPTED`; a definite pre-send connection failure also leaves the recorded
attempt eligible. Uncertain decisions cannot be submitted again through a child alias.
A confirmed invalid destination before dispatch becomes `rejected_transport`; uncertainty
following a possible send remains uncertainty. Delivery acceptance does not complete the task.
A status-message-ID refresh alone does not invalidate an already-recorded decision: dispatch
still requires the exact original task/context, pending payload hash and verified leaf keys,
and never rewrites the receipt's original status snapshot or destination.

Observed tool names alone mint **no merge authorization**. The merge service resolves the
template/provider entry using trusted read-only Session and Agent lookups, then records its
mapping reference with positive consent. The separate merge handler still requires the exact
receipt and proposal gates. Production nested evidence import, provider restoration/retention,
owner-route isolation, and same-turn provider capabilities remain enablement prerequisites. These are fake
transport/PostgreSQL proofs, not deployed Actor restoration. Shared UI controls are described in [inbox](inbox.md) and [chat](chat.md#native-structured-input).

## Shared UI and policy controls

The owner policy response includes `writes_enabled`, derived from the same exact default-off
server gate used by PUT. The project page displays the current value and protected globs even
when editing is disabled. Saving carries the displayed policy version; a conflict or uncertain
save re-reads the current policy before another explicit save.

HITL GET/POST responses include display context from owner-scoped observed/bound session rows,
a verified leaf provider when available, and `writes_enabled`. `GET /sessions/{id}/hitl` lists
up to 100 non-superseded request IDs for that owned session, including read-only receipts and
alias links. Display context never grants response authority. The shared renderer shows a
server-resolved merge summary and bounded proposal details; tool arguments and metadata never
populate that slot. A positive merge response carries the reviewed summary digest in the
immutable Mainloop receipt, and the server checks it against the exact proposal before recording
consent. That digest is removed from the native kagent response. Rejections remain available
without a current summary. This presentation does not add a separate approval queue or merge
execution action; see [Pull requests](pull-requests.md#approval-card-summary) for its sources,
limits, and API contract.

Tests feed real observer/owner-API results from isolated PostgreSQL and a fake gateway into the
shared Svelte renderer and response builder. Seeded states supplement this for provider labels,
escaping, malformed payloads and merge slots. This is not deployed or browser/live-agent proof.

## GitHub plan availability

Mainloop merge policy does not require GitHub paid branch protection or rulesets.
Only the exact plan-unavailable 403 described in [Pull requests](pull-requests.md)
is accepted as absence of rules from that endpoint and recorded in proposal CI evidence.
Readable GitHub rules remain enforced. Mainloop policy, protected paths, complete green CI,
head SHA pinning and approval requirements remain mandatory.

## Coding-task authority and settlement

The task publication source pins task/attempt/workspace/writer generation in existing immutable
merge proposals. Receipt lookup continues to require the exact leaf binding, runtime, proposal,
invocation and argument hash. A parent or sibling task link cannot consume that receipt.
Changed heads require a new proposal; changed bindings, generations or superseded attempts
invalidate execution under the old proposal. No receipt is transferred during handoff.

Successful task completion additionally requires verified merge outcome and fresh successful
CI for the exact proposal head. Preparation and reporting do not imply completion. A lost
merge response keeps the existing intent uncertain until read-only reconciliation establishes
the outcome and exact-head CI; it cannot authorize another write. Task projection/event and
owner outcome notification settlement share the existing merge transaction. Native-runtime
termination and capacity release remain separate, confirmed lifecycle steps.

Task read/cleanup wiring and uncertain-publication handoff holds remain serialized integration
dependencies. The prepared task regressions use fake upstreams and PostgreSQL fixtures; their
execution and technical qualification are pending. No new production activation follows.
