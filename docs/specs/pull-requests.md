# Pull request creation

The `open_pull_request` Mainloop MCP tool is available to authenticated `main` and `child`
bindings and to new owner workspaces enrolled through `POST /workspaces`. Unknown roles,
standalone sessions and retained legacy workspaces cannot discover or invoke it. A checkout or
tool discovery does not enroll a binding.

## Inputs and authority

Inputs are `project_id`, `branch`, `expected_sha` (40 lowercase hexadecimal characters),
`title`, `body`, and a stable `request_id`. The request cannot select a repository URL,
remote, base branch, API host, or token. Branch inputs must be feature branch names, not
revision expressions or fork-qualified heads.

Mainloop resolves the active binding and owner-owned project from PostgreSQL. A child needs its
session's project, repository, and workspace repository to match the project, and its requested
branch to match the workspace branch. A workspace grant additionally requires the exact
project ID, canonical session/workspace repositories, matching session and workspace branches,
a valid feature branch and a live kagent runtime. Missing or inconsistent rows deny the call.
Revoked, archived, finished, deleted or runtime-deleted bindings cannot create PRs. The server
verifies the repository identity and remote branch SHA against GitHub. The head must be in the
project's repository and cannot be its current default branch. The base comes from GitHub's
current default branch, rather than cached project metadata. Identity and authority are
refreshed before creation.

`whoami` reports non-secret grant status and, for an enrolled workspace, its server-resolved
project ID, workspace/session ID, canonical repository and stored branch. It returns
`scope_unavailable` if the grant exists but its scope rows do not agree. An ungranted binding
cannot authenticate to call `whoami`.

Mainloop uses the backend `GITHUB_TOKEN` setting. Requests go only to `https://api.github.com`,
with bounded timeouts, response sizes, and pagination; redirects and environment proxies
are disabled. Each API call has a 15 second total deadline, a 10 second network timeout,
a 2 MB response limit, and PR listing stops at 10 pages of 100 results. Hitting a limit
fails closed. GitHub error bodies, transport errors, and credentials are never returned to
the agent. Returned PR links are constructed from verified repository identity and number.
The REST endpoints are described in [GitHub's pull request API](https://docs.github.com/en/rest/pulls/pulls).

## Deduplication and uncertain results

A PostgreSQL ledger records request IDs and normalized payload hashes per owner, and
serializes creation for each owner/repository ID/head/base tuple. Reusing an ID with another
payload conflicts. Another request ID for the same tuple and payload shares its creation;
a changed payload conflicts. Reusing a branch for a different candidate is therefore not
supported by this slice; use a new feature branch.

Before creating, Mainloop lists matching PRs, including closed PRs, and verifies their
repository IDs, branches, and head SHA. A single matching PR returns `state: created`,
`pr_number`, `url`, `head_sha`, and `base`. Several matches leave the result uncertain;
a mismatching head, base, or fork repository is refused.

Only the caller that first persists an intent may send its single creation POST. No database
transaction spans a GitHub request. After a timeout, crash, or unusable creation response,
retry the same request ID to reconcile by listing the persisted tuple. Mainloop does not
send another creation POST, even if the list is empty: absence cannot prove that an earlier
creation will never appear. A crash before the first POST can likewise leave an intent
uncertain. Uncertain results return `state: uncertain` and the request ID; they do not claim
success or failure. Already recorded results are returned without another creation.

GitHub does not provide a head-SHA precondition for PR creation. Mainloop validates the head
immediately before POST and verifies the response; if the branch changes during creation,
the outcome stays uncertain. Default-branch movement before dispatch refuses creation; no
implicit retargeting occurs.

## Scope and evidence

This tool creates PRs only. It does not merge, edit merge policy, create inbox items, approve
changes, or grant agents GitHub API access. Existing native-session delivery behavior is
unchanged. The default tests use scratch PostgreSQL and a fake GitHub HTTP transport;
they are not live GitHub or deployment proof.

Project policy and the generic decision storage contract are described in
[Merge policy](merge-policy.md). Merge execution and its separate enablement gates are described below.

## Merge tools (implemented, disabled by default)

`MAINLOOP_MERGE_TOOLS_ENABLED=true` is an explicit operator enablement gate. Without
that exact value, merge tools are absent from discovery and invocation is refused.
Production manifests keep it false. Ordinary tools remain at `/mcp`; the separate
`/mcp/merge-approval` surface lists and accepts only
`merge_pull_request_with_approval`. Both require the existing live binding bearer.
Server filtering applies even when a harness ignores binding tool selection.

- `prepare_pull_request_merge(project_id, pr_number, expected_sha, request_id)`
  records an immutable proposal and returns its ID, repository/head/base identities,
  policy/glob versions, normalized file digest, protected matches, CI and required route.
  It creates no human card and does not start the evaluation deadline.
- `merge_pull_request` takes the same inputs. Auto policy with an unprotected diff
  evaluates immediately. Otherwise it returns `approval_required`, the proposal,
  and the protected tool name. Neither path creates a custom approval workflow.
- `merge_pull_request_with_approval(proposal_id, request_id)` requires the exact
  positive immutable HITL receipt for the authenticated leaf binding/runtime,
  canonical merge operation, proposal, invocation ID and argument hash. The receipt
  is claimed with one merge intent. It need not have reached the native runtime yet.

Main, child and enrolled owner-workspace bindings are supported, with the same live
project/workspace authority as PR creation; a workspace must match the PR head to its stored
branch. Standalone observed sessions and legacy workspaces gain no MCP credential or merge
permission. Enrolling a workspace does not enable merge tools or production flags.
Caller-supplied mode, base, URL, token or
`approved` arguments are refused.

### Proposals, decisions and retries

The operator-only `MAINLOOP_MERGE_CONFIGURATIONS` environment value is a bounded JSON
array keyed by `template_name` and `provider`, with the compiled alias, endpoint,
protected tool, `require_approval: true`, and canonical operation. It contains no
binding, Session, or prepared-revision IDs; the default is empty. Before resolving
authority, Mainloop reads the owned kagent Session and its Agent through the trusted
client and uses the Agent's named `templateRef`. Inline templates, unknown or duplicate
template/provider entries, public-name mismatches, and failed reads mint no authority.
Claude and Codex public tool names are derived exactly from the configured alias.

The proposal presentation snapshot and summary digest include a server-computed
template/provider/config-entry digest. Mainloop compares it with fresh Session, Agent,
and config reads when the owner responds; a changed or missing mapping blocks positive
approval while leaving rejection available. Legacy per-session config is rejected and
pending requests using it must be reissued. The template contents and RemoteMCPServer
identity are not pinned by this simple mapping, so a reviewed template edit can redirect
the alias. GitOps review, protected-route filtering, and the recorded-consent gate still
apply. No API accepts these records from an actor.

The existing HITL response route resolves these records and proposal facts before
recording a merge decision. Recognized proposals and policy changes serialize with
merge claims on the project and per-PR locks. Rejection of any recognized proposal
bars every further attempt for that candidate/head, including auto and new request
IDs. Generic tool decisions with similar arguments impose no merge barrier or consent.
An already-claimed intent cannot be retroactively rejected. Approving stale facts
fails; rejecting stale owned proposals remains possible before claim.

The owner API returns server-resolved merge context keyed by tool/proposal ID in its
bounded `merge` display array. Raw inventories stay behind the owner-scoped details route.
The shared inbox/chat renderer shows each call's repository/PR, exact
head/base branches and SHAs, protected paths, and recorded CI checks/statuses.
This is read-only evidence, not approval or a promise of current CI success; it adds
no actions. Missing, malformed or ambiguous evidence is shown as unavailable, and
stale observations/proposals label displayed facts as historical. Agent-supplied
names may trigger an unavailable notice but cannot supply verified merge context.
Staleness includes proposal replacement/policy changes; GitHub head/base/diff freshness
is checked on submission and invocation, not inferred from a stored card.

### Approval-card summary

Each immutable proposal also stores a deterministic presentation snapshot. It uses the
verified GitHub PR title and bounded plain-text description, the complete changed-file
inventory and per-file additions/deletions, protected-path matches, policy/glob versions,
and the captured CI inventory. No model writes this summary. The native HITL hint remains
separate and is labelled as an agent note.

The snapshot includes the repository and PR, exact head/base refs and SHAs, proposal ID,
description digest, changed-path digest and statistics, approval reasons, CI inventory
digest/completeness/time, and policy versions. Mainloop stores its canonical SHA-256 digest
with the immutable proposal. The owner card labels CI as historical evidence and says that
it does not establish that a merge is safe or complete. Current evidence is read again by
the response and merge paths.

`GET /hitl/{request_id}` returns the summary, digest, freshness state, and verified PR,
compare, and conversation links. The owner-only read route
`GET /hitl/{request_id}/merge/{proposal_id}/details` accepts `section=description|files|checks`,
an offset cursor, and a page size of at most 50 records. Pages are limited to 64 KiB. Saved
PR descriptions are limited to 16 KiB; changed-file inventories retain the existing 3,000
file cap. The UI renders descriptions as escaped text and does not store patches or raw
GitHub response bodies. Truncated details carry an explicit marker and digest/count.

`POST /hitl/{request_id}/respond` may include `reviewed_context`, a map from native tool ID
to summary digest. Positive merge decisions require the digest currently stored with the
exact proposal and a complete fresh GitHub evidence read. Missing context, a changed digest,
an unavailable summary, a superseded proposal, or a changed pending leaf refuses the positive
decision before consent is recorded. Rejections do not require the digest. This field is
Mainloop receipt metadata and is stripped from the native kagent response. The existing merge
path still checks current policy, identity, diff, CI and mergeability before any merge intent.

A 30-minute deadline starts on the first authorized evaluation, after human waiting.
Repeated invocations continue the same attempt without extending it. Pending CI or
unknown/unready mergeability returns `evaluating`; callers reuse the invocation ID
to refresh. No new polling worker or scheduler is introduced. Failed checks close
an attempt as `blocked`; expiry closes it as `expired`. Explicit preparation with
a new request ID replaces a conclusively closed attempt with a new immutable proposal.
Approval retries need a new native call and owner decision. Repeated preparation IDs
return their original proposal and cannot reset a deadline or reopen rejection. Immutable per-proposal terminal results retain earlier blocked/expired outcomes across later attempts.

### Evidence and dispatch

Evidence requires an open non-draft same-repository feature PR at the expected SHA,
current default-branch base and squash support. Complete paths are read between
matching PR/repository observations, including deletion and rename sources. More
than 3,000 files, malformed paths, pagination/count mismatches or duplicate paths
fail closed. A complete evidence refresh has a 60-second bound (also shared across an owner decision batch). Protected globs and versions come from the server policy contract.

Exact-head runs and statuses are paginated separately. Check-suite enumeration must
prove fewer than 1,000 suites; check runs and statuses are bounded at 1,000 records.
A cap or incomplete inventory fails closed. Check reruns are ordered by numeric ID
within `(app ID, name)`: the REST run schema lacks a creation timestamp, and
`started_at` can be absent for queued runs. This documented ordering ensures a newer
queued run defeats old success. Statuses use creation time then ID per context.
Every enumerated completed suite must have conclusion `success`, whether or not
it emitted runs; a newer successful run or suite does not override an unsuccessful
historical suite. Missing, unknown, cancelled, neutral and other non-success suite
conclusions block merging. Normalized suite outcomes are retained in CI evidence.
Every selected run must be completed/success and every selected status success;
neutral/skipped/unknown and empty overall evidence are never green. Required named
and app-bound checks must exist. Check and status namespaces remain separate.
Supported nonterminal suite/run states are `queued`, `in_progress`, `pending`,
`waiting` and `requested`. They keep the existing bounded evaluation active, including
when a run starts between the suite and run reads; completion within the original
deadline can use the same proposal and owner receipt. Unknown states are not treated
as pending. Terminal unsuccessful evidence takes precedence over pending evidence.
See [GitHub check-run API](https://docs.github.com/en/rest/checks/runs).

Classic protection returning 404 is supported as no classic protection; ruleset-only
and unprotected branches can proceed. This exception applies only to that endpoint:
repository, branch, PR, CI and active branch-rule inventories must still succeed.
An HTTP 403 with the exact GitHub message "Upgrade to GitHub Pro or make this
repository public to enable this feature." on either branch-rule read is treated as
no rules from that endpoint. The CI proposal evidence records the affected endpoints
in `github_rules_unavailable_on_plan` (`protection` and/or `rules`), so summaries can
identify plan limitations. Readable rules from the other endpoint still apply.
Other protection and rules errors, including permission failures and malformed responses,
fail closed. Since a 404 can also conceal missing access,
operators must supply appropriate read permissions; GitHub remains the enforcement
boundary at merge time.

Classic and active ruleset required status checks both add required named/app-bound
evidence. Active `deletion`, `non_fast_forward` and `required_linear_history` rules
are compatible with the PR squash API. A `pull_request` rule with zero required
approvals is supported only with all four required review flags explicitly false.
Missing flags, stale-review dismissal, code-owner review,
last-push approval, review-thread resolution, nonzero approval counts, unknown PR
parameters and merge-method lists excluding squash fail closed with an unsupported
PR-rule reason. Ruleset checks require an explicit
`strict_required_status_checks_policy: false`; true is unsupported because Mainloop
does not prove the head was tested with the latest base. Classic `strict: true`
is likewise unsupported. Unknown active rule types also fail closed.

Classic checks require both `contexts` and `checks`; every check must include
`app_id`. An explicit null means any app, but an omitted ID is rejected.
Classic review and push restrictions, conversation resolution, required signatures,
branch locking and creation blocking remain unsupported. Unknown protection fields
and malformed flags fail closed. Linear history is satisfied by squash; admin
enforcement applies the same evaluated requirements. Force-push, deletion and fork-sync
permissions impose no additional requirement on this PR squash operation.
No rule grants Mainloop merge authority
or relaxes its CI requirements, including the refusal of empty overall evidence.

Fresh evidence, current policy, proposal identity and known mergeability precede the
transactional intent. The single PUT uses `merge_method=squash` and
`sha=expected_head_sha`. No transaction spans network calls. Durable uncertainty is
written before dispatch. A crash before sending, lost response, upstream refusal or
unusable response permits observation only, never another PUT or lease-expiry replay.
Exact matching merged-PR observation may resolve the outcome; this reports GitHub's
outcome, not proof that Mainloop was the actor. Terminal success and one deterministic
informational inbox notification share a transaction. Duplicate callers receive the
stored result and cannot duplicate publication.

Head pinning does not make base/check observations atomic with GitHub's merge API.
External base retargeting and CI changes in the final read/PUT gap remain limitations;
stronger guarantees require GitHub enforcement. These fixtures are not live proof.

### Enablement remains separate

Before enabling either binding/tool or owner write routes, verify both repository
revisions, exact-head remote CI, direct and gateway owner-route isolation, GitHub
credential confinement, both route credentials, per-template compiled mappings and
retained native-session compatibility. Prove provider full-snapshot pause/restore and
retention separately. Manifest binding/server additions are commented candidates,
not rendered production resources. Deploy only through GitOps after those gates.

Workspace branch mismatches report "branch does not match this workspace"; a mismatch
with the stored project default branch reports "default branch is not allowed".
These reasons are returned only after verifying the owned project and workspace scope;
missing or unauthorized bindings retain the generic ownership refusal. Live GitHub default
branch checks still reject a default PR head.

## Durable coding-task publication (source implementation; qualification pending)

For a delegated coding task, publication is bound to its current attempt, native binding,
workspace and writer generation. The PR creation intent remains in `pr_creations`; its ID
is referenced from that attempt's existing evidence references before dispatch. A verified
result updates `TaskProjection` through the existing atomic projection/event helper.
An existing intent without that exact attempt association is refused, including a successor
trying to adopt an uncertain source intent. No task publication table is introduced.

Immutable merge proposals additionally pin task, attempt, workspace and writer generation.
Preparation, evaluation and the one external write take the same policy/tree/publication/runtime
locks as source revocation. A stale or revoked attempt cannot claim a new merge. Auto still
uses the existing merge service; protected paths and Approval still require the original
leaf's exact owner receipt. Task summaries and reports supply neither publication nor consent.

CI evidence records the exact head, collection timestamp, completeness and pending/failure
state. Missing checks, unreadable evidence, a mismatched head, a future timestamp or evidence
older than five minutes are unknown. Unknown and pending evidence cannot complete a task.
The durable merge intent retains the successful exact-head evidence accepted at dispatch;
reconciliation of a lost response reads the PR and fresh exact-head checks without another PUT.
A later policy edit cannot undo a merge already admitted under the recorded policy.

Verified merge settlement updates the leaf task and projection, reserves its task event and
one parent update, and uses the existing deduplicated owner merge notification in the same
transaction. It does not complete the parent coding task, delete a native runtime, release a
branch claim or free runtime capacity. Runtime fencing/cleanup remains a separate lifecycle
operation. Closed without merge is a separate observed PR state.

The read-only projection refresh port performs bounded GitHub reads and never sends native
turns or merges. Its installation and stale-CI read masking are reserved for serialized shared
service integration. The initial PR-intent claim and attempt association currently use two
transactions: a crash between them leaves an unassociated uncertain intent that fails closed.
Atomic claim/association and handoff holds for uncertain PR/merge intents require the shared
integration contracts before production qualification. Prepared offline/PostgreSQL tests are
not executed or live proof. Existing production enablement flags remain unchanged.
