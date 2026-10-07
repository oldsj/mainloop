# Git publication authority

## Implemented: disabled standalone contract

`PUSH_GATE_ENABLED` defaults to `false`. There is no Git listener, upstream writer,
credential injection or enforcement on actor Git traffic in this slice. The origin and
command/body limits are reserved settings, not an advertised service. Actors still hold
today's GitHub credential until infrastructure replaces it with read-only PATs. These
primitives do not establish a deployed publication boundary.

Push grants are purpose-separated random bearers stored only as SHA-256 hashes. They
confer no MCP or owner API authority. One stable grant ID equals its workspace/session ID; revoked grants can be reissued
only at the next version with a fresh bearer. Audit rows survive session deletion.
A grant binds owner, project, canonical GitHub
repository, exact case-sensitive branch, workspace/session and current runtime identity.
Only a live `agent` binding with workspace grant kind, active MCP enrollment, matching
stored workspace and session, and a nonarchived/nonterminal session qualifies. Runtime
identity currently uses the stored kagent Session ID; attested runtime UID validation is
still required before activation.

The pure authorization function permits exactly one `refs/heads/<stored branch>` update,
creation or trusted fast-forward. Repository identity is case-insensitive; branch identity
is case-sensitive. Default and configured protected branches override the allowlist.
Tags, notes, deletion, multiple refs and divergent/rewinding updates are denied with stable
reason codes. Missing ancestry or metadata fails closed. Creation authorization is only
a scope decision: connected commit/object/pack validation is owed by the future listener.
LFS uploads are outside this contract.

Per-project policy versions increase by one. Patterns use case-sensitive shell glob
matching (`release/*` matches nested names). Previously observed default branches remain
protected; no default-release API is supplied yet. The policy is independent of merge
policy and protected file paths. Policy setup requires agreement with stored repository default metadata; changed or
unavailable metadata refuses authorization until a new policy version records it. Before activation, refresh canonical identity/default immediately before each
dispatch and persist newly observed defaults. GitHub administrator changes cannot be
transactionally locked with publication; the external-admin/default-rename race is an
accepted operating limitation, even with fresh metadata.

`push_gate.store` owns cross-process PostgreSQL advisory locks. Publication takes the
project policy lock then the grant lock and keeps them through outcome recording. Lifecycle
revocation waits on the same grant lock. A dispatched write cannot be recalled. Use a
dedicated connection; never return a locked connection to the pool. No transaction is
held during network I/O. Policy updates serialize through the project lock.

The publication ledger preserves request identity, repository/ref, old/new OIDs and
policy/grant versions. Allowed transitions are `pending` to `dispatching` or `rejected`,
and `dispatching` to `confirmed`, `rejected` or `unknown`. Terminal states cannot be
reopened. An unresolved `dispatching` or `unknown` attempt blocks new authorization
for that grant, including after bearer rotation. Request identity reuse with different facts is refused. `unknown` is never
retried automatically; future reconciliation requires remote evidence and owner resolution.
A process crash leaving `dispatching` must be reconciled to unknown, never replayed.

## Deferred: slice 1b and activation

Workspace lifecycle wiring and read-only API/UI exposure are deferred to slice 1b.
Protected-branch workspaces remain readable but must receive no push grant. The current
workspace API has no new publication status yet. Issue/revoke/runtime-replacement
primitives exist for that integration, but no lifecycle caller uses them today.

Before enabling any listener: wire and test lifecycle locks; replace actor GitHub write
PATs with read-only PATs; isolate service-only write PATs; close actor access to kagent
control APIs, secret resolution and owner routes; bind verified runtime identity;
implement bounded smart-HTTP parsing, disk spooling,
trusted pack/ancestry validation and upstream receipt handling. Fixture/PostgreSQL tests
prove local primitives only, not these deployment requirements. Merge authority remains a
separate route with its existing gates.

Deployment is greenfield: no legacy session or retained-binding migration is planned.
