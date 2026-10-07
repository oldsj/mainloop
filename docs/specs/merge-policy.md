# Merge policy and HITL storage contracts

Implemented foundation: project policy API, immutable protected-path rules, typed HITL
payloads, observed-session/checkpoint storage, verified alias storage, and durable owner
decision receipts. This does not implement merge execution, background HITL discovery,
owner decision submission, continuation transport, or inbox/chat controls. Those consumers
must satisfy the contracts below before enabling answers or merge tools.

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
This helper establishes path policy only; the future merge service must prove GitHub path
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
produce an actionable receipt. The existing gateway decoder is not yet wired to these types.

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
`accepted`, `uncertain`, `rejected_transport`); this foundation performs no remote dispatch.

## Frozen merge receipt contract

`canonical_operation(public_name: str, configuration: VerifiedLeafConfiguration | None)
-> TrustedToolMapping | None` performs exact matching against one verified prepared-revision
snapshot. Configuration includes owner, binding, runtime, provider, revision and evidence
reference. A mapping includes compiled alias, RemoteMCPServer identity, endpoint, selected
`merge_pull_request_with_approval` tool, `require_approval=True`, and the canonical operation
`mainloop.merge_pull_request_with_approval.v1`. Unknown revision, alias, or ambiguous mapping
returns no merge authority. No live configuration snapshot importer is implemented here.

For the verified alias `mainloop-merge-approval`, the only valid public names are:

- Claude: `mcp__mainloop-merge-approval__merge_pull_request_with_approval`
- Codex: `mainloop-merge-approval.merge_pull_request_with_approval`

Other aliases require explicit pinned configuration entries. Bare names, suffix matches,
another server's same tool, and other tools with identical proposal arguments remain generic.
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
identity from its own protected registry. The future proposal resolver must validate owner,
binding and immutable proposal facts when building a receipt. Its callback contract is
`Callable[[MergeReceiptKey, bool], None]`, where the boolean is the per-call approval;
it raises on failure, allows rejection of stale owned proposals, and refuses stale approval.
The merge service must claim the positive receipt with one persisted intent, serialize with
policy/rejection gates, and recheck all current authority, candidate and GitHub evidence.

Recorded consent does **not** attest that a later raw invocation traversed native approval.
Lookup intentionally does not wait for transport acceptance, so the resumed tool cannot
deadlock on its own continuation. A raw call after consent can only be allowed for the exact
operation, once, by the future merge-intent gate. No live native restoration or provider
capability parity is claimed by the fixtures.
