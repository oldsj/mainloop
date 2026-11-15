# Privileged actions

Status: **proposed**. Nothing here is implemented. It builds on implemented pieces: merge proposals
and receipts ([merge policy](../specs/merge-policy.md), [pull requests](../specs/pull-requests.md)),
Needs-you attention and HITL cards ([inbox](../specs/inbox.md)), ntfy push
([push notifications](../specs/push-notifications.md)), the `watch_checks` wake ([CI](../specs/ci.md)),
per-binding MCP credentials ([credentials](../specs/credentials.md)) and owner API authentication
([authentication](../specs/authentication.md)).

## Problem

The owner still runs the credentialed steps of agent work by hand: admin merges and PR closes with
`gh`, 1Password lookups with `op`, `kubectl` writes on internal-01 (rollout restarts, server-side
applies, patches, deleting a RoleBinding), Talos and Omni applies, `sudo`. Agents cannot do these,
correctly: they hold no credentials, and Claude's auto-mode classifier holds such commands even
where a credential exists. This is the main thing keeping the owner in a terminal rather than in
Mainloop.

Goal: an agent **requests** a privileged action; the owner approves it from the phone, seeing
exactly what will run; an executor that holds the credential runs exactly that; the result goes
back to the requesting session. Agents never hold the credential, and secrets never reach a
transcript.

## 1. Action model: a typed catalog, not a shell

Each privileged action is a catalog entry, versioned and compiled into Mainloop and the executor:

| Field        | Meaning                                                                                       |
| ------------ | --------------------------------------------------------------------------------------------- |
| `type`       | Stable name, such as `k8s.rollout_restart`, `github.pr_merge`                                 |
| `version`    | Bumped when arguments, preflight, rendering or execution change                               |
| `args`       | JSON Schema; validated at the MCP boundary, canonicalized (defaults filled, keys sorted)      |
| `preflight`  | Read-only step run by the executor before approval; returns frozen facts (see §2)             |
| `render`     | Builds the card from args and frozen facts only; never from agent prose                       |
| `execute`    | The one write, with remote preconditions from the frozen facts                                |
| `observe`    | Reads remote state to decide whether the write happened (see §5)                              |
| `credential` | Which executor credential it uses (see §3)                                                    |
| `floor`      | Hard policy floor: `ask_always` or `auto_allowed` (see §4); a server constant, not owner data |

Starting catalog, from what the owner actually runs:

| Type                               | Args (abridged)                                                   | Preflight freezes                                                            |
| ---------------------------------- | ----------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `github.pr_merge`                  | repo, pr, expected_head_sha, method, `bypass_rules`               | head/base SHAs, checks, reviews, files changed, which rules the bypass skips |
| `github.pr_close`                  | repo, prs (≤ 20), comment                                         | each PR's title, author, state, head SHA                                     |
| `k8s.rollout_restart`, `k8s.scale` | cluster, namespace, kind, name, (replicas)                        | object UID, resourceVersion, current replicas, Argo tracking                 |
| `k8s.patch`                        | cluster, namespace, kind, name, patch type, patch                 | UID, resourceVersion, server dry-run result and diff                         |
| `k8s.apply`                        | cluster, namespace, manifests (inline, ≤ 64 KiB), force_conflicts | server-side-apply dry-run per object and diff against live, Argo tracking    |
| `k8s.delete`                       | cluster, namespace, kind, name                                    | UID (used as the delete precondition), owner references, finalizers          |
| `argocd.sync`                      | app, prune (default false)                                        | app revision, out-of-sync resources                                          |
| `onepassword.item_to_secret`       | item ref (vault/item/field), target namespace, Secret name, key   | item and field exist (metadata only), target Secret's current owner/labels   |

Raw shell is **out**, including "raw command with approval":

- **It cannot be reviewed on a phone.** `kubectl apply -f /tmp/x.yaml` shows a path, not what it
  does; shell expansion, pipes and `$(op read …)` hide the meaning. A typed action shows the
  server's dry-run diff.
- **It cannot be bound.** A digest of a command string does not pin the file it reads or the state
  it runs against. Typed preflight facts (resourceVersion, head SHA, UID) become preconditions.
- **It cannot be least-privilege.** A shell needs the union of every credential; a typed action
  names one.
- **It cannot keep secrets out.** Output of an arbitrary command can carry anything; a typed result
  has a schema.
- **It cannot be reconciled.** After a timeout there is no way to observe whether an arbitrary
  command ran; each typed action has an `observe` step.

A request for something outside the catalog fails with `unsupported_action`; the agent reports
blocked with the exact command, and the owner runs it as today. Repeated asks for the same missing
action are the signal to add a catalog entry, which is a reviewed code change.

`gh auth`, Claude's own permission prompts and `sudo` on the owner's Mac are not privileged actions:
agents in Mainloop have no `gh` session to authenticate, harness prompts stay on the existing HITL
path, and the executor cannot reach the Mac.

## 2. Approval binding

### Request

```text
request_privileged_action(type, args, reason, request_id) ->
  { action_id, state, expires_at, summary }
```

Mainloop takes the requester's identity (binding, session, task, role, depth) from MCP
authentication, never from arguments. It validates and canonicalizes `args`, writes the intent row
(§5), and returns at once. `request_id` makes the call idempotent per session, as in
`watch_checks`. `reason` is agent text: stored, shown labelled as the agent's claim, and excluded
from the digest's meaning.

The executor then runs `preflight` and stores the **frozen facts**. The request becomes a
**proposal** with an immutable digest:

```text
digest = SHA-256(canonical JSON {
  type, version, args, frozen_facts, requester binding/session/task,
  action_id, expires_at, executor catalog version })
```

The same canonical JSON rules as merge proposals apply (UTF-8, sorted keys, compact, no NaN).

### What the owner sees

Policy routes the proposal (§4). On `ask`, Mainloop creates an attention item (kind `approval`,
source `privileged_action`, item id `pa:<action_id>`) linked to the requesting task, and the
existing ntfy sweep pushes it at priority 4. The push opens the card; the card is the only place
to approve. ntfy action buttons are not used: an approval must come through the authenticated owner
API with the card's digest.

The card is rendered by the server from args and frozen facts:

- a one-line title from the catalog (`Restart deployment/snmp-exporter in monitoring on
internal-01`), never the agent's words;
- the exact target and, where there is one, the diff (dry-run for `k8s.*`; commits, files and
  checks for merges);
- risk tags from the catalog and facts: `destructive`, `bypasses branch rules`, `writes a Secret`,
  `Argo-managed: will drift or be reverted`, `irreversible`;
- the requester chain (task, supervisor, main) and the agent's reason, set apart and labelled;
- expiry, and the policy rule that sent it to the owner.

Buttons: **Approve**, **Deny** (optional reason), and where the floor allows it, **Approve and allow
this target for 24 h** (§4). Approve sends `reviewed_context: <digest>`, like a merge approval; a
digest that no longer matches the stored proposal refuses the approval.

### Binding rules

- **Exact.** The decision names the digest. The executor recomputes the digest from the stored
  canonical args and facts before running and refuses a mismatch.
- **One-shot.** A unique constraint lets one approval be claimed by one execution. A retry after an
  uncertain outcome is the same execution (§5), never a second claim.
- **Preconditions.** Execution carries the frozen facts as remote preconditions: `resourceVersion`
  for patch and scale, UID for delete, `sha` for merge. Changed remote state fails as `stale`,
  and the agent must request again; nothing is re-planned silently.
- **Expiry.** A proposal not decided within 24 hours (catalog may shorten it; `bypass_rules` merges
  and deletes: 1 hour) expires. An approved proposal not claimed by the executor within 5 minutes
  expires too, so a backlog never runs old approvals late.
- **Deny.** Recorded with the owner's reason, which is delivered to the agent.
- **Edit.** Not in the first version. The owner denies with a note ("scale to 2, not 0") and the
  agent requests again; an edited action is a new proposal with a new preflight and digest, so the
  owner always approves what preflight saw.

### What the agent sees while waiting

The `watch_checks` pattern: the tool returns `pending_approval`, the agent ends its turn, and its
workspace may sleep. When the action reaches a final state (succeeded, failed, stale, denied,
expired), Mainloop sends the session one message, idempotent on the action id, with the outcome and
the structured result. The task stays `running` with an idle session; the liveness sweep treats it
like an open watch, and the task shows "waiting for approval" with a link to the card.
`get_privileged_action(action_id)` gives a snapshot; `cancel_privileged_action` withdraws an
undecided request.

### Why not kagent `requireApproval`

The ops-agent spec planned writes as tools bound with `requireApproval`, the path merge approval
uses. This design uses Mainloop's own approval instead:

- A deferred tool call keeps the turn's delivery open, and later messages queue behind it. For main
  that means the owner cannot talk to main while an approval is pending.
- Binding consent to a native tool call is the costly part of merge policy (template/provider
  mapping, alias verification, receipt lookup), and that page still records that consent does not
  attest a later raw invocation passed through native approval.
- Every requesting Agent would need the protected RemoteMCPServer in its template.
- The card needs server-resolved facts (diff, head SHA), so Mainloop builds a proposal either way.

So the ops agent's slice 2 should request writes through this catalog, and its tool server stays
read-only. The native permission path stays for harness prompts. Whether merge approval should
later move onto this mechanism is an open question.

## 3. Executor

**Where.** A new subcommand, `mainloop serve-executor`, deployed through GitOps as its own
Deployment in its own namespace (`mainloop-executor`) on internal-01. It has **no network ingress**:
it takes work from PostgreSQL (`LISTEN` plus polling) and writes results back. Egress is limited to
PostgreSQL, the Kubernetes API, `api.github.com` and 1Password. No workspace, Session or MCP server
can reach it. Its credentials are mounted only there; the API process never holds them.

**Database.** Its own role: `SELECT` on proposals and decisions, `UPDATE` only on the execution
columns, `INSERT` on the event log. The API role cannot write execution state.

**Credentials, least privilege per action type:**

| Credential | Holds                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GitHub     | A **separate** GitHub App (`mainloop-privileged`), installed only on the repositories the owner picks, added as a ruleset bypass actor only where `bypass_rules` should work. Tokens minted per call, one repository, only the permissions the action needs. Mainloop's normal App keeps no bypass.                                                                                                                                                            |
| Kubernetes | The executor's ServiceAccount can only `create serviceaccounts/token` for a few narrow ServiceAccounts: `executor-workloads` (patch/scale/restart/apply on an allowlist of kinds), `executor-delete`, `executor-secret-writer` (create/update Secrets in allowlisted namespaces, no get/list), `executor-argo` (patch Applications). Each execution mints a 10-minute token for the one it needs. No RBAC kinds, no Secret reads, no `exec`, no cluster-admin. |
| 1Password  | A 1Password Service Account, read-only, scoped to named vaults. Not the Connect operator's token.                                                                                                                                                                                                                                                                                                                                                              |

Kinds that change the authority boundary are not in any allowlist: RBAC except deleting a
RoleBinding or ClusterRoleBinding (delete needs no `escalate`), Secrets except through
`onepassword.item_to_secret`, the `mainloop`, `mainloop-executor`, `kagent`, `argocd` and
`kube-system` namespaces, and admission webhooks. Changing those stays a GitOps change the owner
merges.

**Secrets never return.** Each type's result has a schema with no free-form value. For
`onepassword.item_to_secret` the executor reads the field into memory, writes it into the Secret
with server-side apply (field manager `mainloop-executor`, annotation
`mainloop.dev/privileged-action: <id>`) and returns the Secret's name, key and resourceVersion,
never the value or a hash of it. The value is not logged or stored in PostgreSQL. Error text has
any exact occurrence of the value removed (the executor knows it; a pattern redactor would miss a
bare value), then goes through the session redactor (`server/internal/sessions/redact.go`) and a
length bound before it reaches the result. Where the
value should live long-term, the better path is a `OnePasswordItem` in the infrastructure
repository, which the Connect operator already syncs; this action covers what that path cannot
(another vault, a one-off target).

**Audit.** An append-only `privileged_action_events` table: requested, preflighted (facts digest),
routed (rule id and version), decided (owner principal, client, decision, digest), claimed
(executor instance, fence), executed (result), observed, delivered. Remote objects carry the action
id (annotation on Kubernetes objects; a comment on the PR for merges and closes, naming the
approval). An owner page lists actions and their events. Rows are never updated or deleted by
Mainloop's normal roles.

## 4. Policy

Policy decides `auto`, `ask` or `deny` for each proposal. It is owner data, versioned with an
immutable audit row per change, like merge policy:

```text
rule: { type, target selector (cluster/namespace/name glob, repo, PR author),
        requester roles, decision: auto|ask|deny, expires_at? }
```

- **Default: everything asks.** No rule auto-approves until the owner writes one.
- **Hard floor.** `ask_always` types can never be auto-approved, whatever the rules say: every
  delete, `bypass_rules` merges, `k8s.apply`, `k8s.patch`, `onepassword.*`, anything tagged
  `Argo-managed`. The floor is compiled into both the API and the executor, and the executor
  refuses an auto decision on a floor type even if the API recorded one.
- **Pre-approvable candidates:** `k8s.rollout_restart` and `k8s.scale` (within bounds, not to 0)
  outside the protected namespaces, `argocd.sync` without prune, `github.pr_close` of PRs authored
  by Mainloop's App or Renovate.
- **"Approve and allow this target for 24 h"** writes a rule for that exact type and target with an
  expiry. It is offered only for non-floor types.

**Who can request.** Requests are harmless on their own, but every one costs owner attention and is
an injection target.

| Role                                     | Can request                                                    |
| ---------------------------------------- | -------------------------------------------------------------- |
| Main thread                              | Any catalog type                                               |
| Supervisors (depth 1)                    | Any catalog type                                               |
| Ops agent                                | Any catalog type                                               |
| Code and coordination children (depth 2) | Nothing; they report blocked to their supervisor, who requests |

Children read the most untrusted input (dependency code, PR comments, logs) and have the narrowest
view, so keeping them out shrinks the injection surface without losing much. Auto rules also match
on requester role, so a rule can allow the ops agent to restart a Deployment without allowing a
supervisor to.

**Limits.** At most 3 undecided requests per session and 10 overall; more fail with
`too_many_pending`. A request whose canonical args match an undecided one from the same session
returns that one.

## 5. Durable state

Tables (names indicative):

- `privileged_actions`: the intent. `id`, owner, requester binding/session/task/role,
  `(session_id, request_id)` unique, `type`, `version`, canonical `args`, `reason`, `state`,
  `expires_at`, `frozen_facts`, `digest` (both immutable once written), execution columns
  (`claim_fence`, `claimed_by`, `execution_started_at`, `result`, `outcome`), delivery columns.
- `privileged_action_decisions`: immutable. `action_id` unique, decision, digest, principal, auth
  mode, rule id/version for auto decisions, reason, time.
- `privileged_action_events`: append-only audit (§3).
- `privileged_action_rules` and its audit: policy (§4).

States:

```text
requested -> preflighting -> pending_approval -> approved -> executing -> succeeded
                         \-> preflight_failed  \-> denied           \-> failed | stale
                                               \-> expired          \-> uncertain -> (observed) succeeded | failed
                                               \-> cancelled
          (auto rule) pending_approval -> approved
```

Delivery to the session is separate (`delivering`, `delivered`, `undeliverable`), as for check
watches, with the action id as the message identity.

The reconciler rule:

- **Intent first.** The tool commits the row before anything else; no network I/O in that
  transaction.
- **Claim, commit, then I/O.** The executor claims a row with a fence and commits
  `executing` and `execution_started_at` before calling the remote. No lock or transaction is held
  across the call. One execution per target at a time (a target key such as
  `k8s/internal-01/monitoring/Deployment/snmp-exporter`).
- **Idempotency key.** The action id, carried into the remote where the remote allows it:
  annotation on Kubernetes objects; `kubectl.kubernetes.io/restartedAt` set to the approval time,
  not now; the merge comment; the Secret's annotation.
- **Observe before retry.** After a timeout or crash the outcome is `uncertain`, and the executor
  runs `observe`: PR merged at the expected head; restart annotation equals the action's value;
  object gone or UID changed; Secret annotation names the action. Done counts as `succeeded`.
  Not done, with preconditions still holding, retries only for types marked `retry_safe`
  (restart, scale, patch with resourceVersion, Secret apply). Otherwise it stays `uncertain` and
  becomes an owner attention item. Nothing is replayed blindly.

## 6. Slices

**Prerequisite.** Owner API authentication in `enforce` (#420, #422 rollout). An approval must be
attributable to the owner; the enable gate (`MAINLOOP_PRIVILEGED_ACTIONS_ENABLED`, exact `true`)
refuses to start without `MAINLOOP_AUTH_MODE=enforce`, and the executor refuses decisions not
recorded under `enforce`.

**Slice 1: the mechanism, plus GitHub.** Tables, the three MCP tools, the card and its attention
source, push, the wake message, the executor with no ingress, the audit page, and `github.pr_close`
and `github.pr_merge` (with `bypass_rules`). GitHub merges and closes are the most frequent
credentialed steps the owner runs, the credential is one App key with no cluster RBAC to design,
and preflight reuses the merge service's fact reading. Normal merges of Mainloop project
repositories keep using `merge_pull_request` under merge policy; `github.pr_merge` is for
`bypass_rules` and repositories that are not Mainloop projects.

**Slice 2: Kubernetes on internal-01.** `k8s.rollout_restart`, `k8s.scale`, `k8s.patch`,
`k8s.delete`, `k8s.apply`, `argocd.sync`, with the token-minting ServiceAccounts. The ops agent
requests writes through these, replacing its planned `requireApproval` tools.

**Slice 3: 1Password.** `onepassword.item_to_secret`. Ordered after Kubernetes because the Connect
operator already covers the GitOps case, and many `op` reads were feeding a `kubectl` or `curl`
that a typed action now runs whole.

**Slice 4: policy and stronger approvals.** Owner-written auto rules and "allow this target for
24 h"; passkey-signed approvals (below).

**Not planned.** Raw shell, `sudo` on the Mac, database writes through `kubectl exec` (see open
questions), Talos and Omni (later typed actions such as `talos.reboot` and `omni.apply` with a
config diff, once the pattern is proven; their blast radius is the whole cluster).

## 7. Threats

| Threat                                                                                                                           | Mitigation                                                                                                                                                                                                                                                                                                                            |
| -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Prompt injection asks for an action** (a PR comment or log line tells the agent to read a secret into a namespace it controls) | The card shows the target and diff from server facts, never agent prose; the reason is labelled as the agent's; children cannot request; Secret writes go only to allowlisted namespaces; secret-moving types are on the hard floor. The residual risk is the owner approving a well-disguised request, which the next row addresses. |
| **Approval fatigue** (the owner taps Approve on everything)                                                                      | Batched `github.pr_close` (one card for several PRs); idempotent duplicates; pending limits; auto rules for low-risk targets so cards are rare and mean something; risk tags; floor types look different on the card. A weekly digest lists auto-approved actions.                                                                    |
| **Replay**                                                                                                                       | One-shot claim on a unique decision; digest includes the action id and expiry; remote preconditions from frozen facts; approved-but-unclaimed expiry of 5 minutes.                                                                                                                                                                    |
| **Compromised workspace**                                                                                                        | It holds only its MCP bearer. It can request (bounded by role and limits), not approve or execute. It cannot reach the executor, which has no ingress. Revoking the binding stops its requests.                                                                                                                                       |
| **Spoofed approval through the owner API**                                                                                       | Requires `enforce` owner auth; decisions record the verified principal; the executor rechecks principal, auth mode, digest and floor.                                                                                                                                                                                                 |
| **Compromised Mainloop API process**                                                                                             | It holds no executor credential, but it can write decision rows, so it could approve. Residual risk until approvals are signed: slice 4 makes Approve a WebAuthn assertion over the digest from the owner's passkey, which the executor verifies with a registered public key, so a forged row fails.                                 |
| **Catalog escalation** (an approved `k8s.apply` creates a pod that mounts a ServiceAccount token or Secret)                      | Apply kinds are an allowlist, RBAC and protected namespaces are excluded, `k8s.apply` is always ask, and the card shows the full object diff, with pod specs that mount Secrets or tokens tagged.                                                                                                                                     |
| **Argo drift**                                                                                                                   | Preflight detects Argo tracking; the card says the change will be reverted or will drift; floor ask. GitOps stays the way to make lasting changes.                                                                                                                                                                                    |

## Open questions for the owner

1. **Second GitHub App for bypass**, or add Mainloop's existing App as a bypass actor on chosen
   repositories? A second App keeps the API process unable to bypass; it is one more key to manage.
2. **Which repositories** get `bypass_rules` at all (mainloop, infrastructure, opencozy)?
3. **Edit on the card.** Is deny-with-note enough, or should a few fields (replicas, comment text)
   be editable on the card, at the cost of a second preflight before approval?
4. **Expiry.** 24 hours for undecided requests and 1 hour for bypass merges and deletes: right?
5. **Slice order.** Kubernetes before 1Password, or the reverse?
6. **Database writes.** The owner has fixed rows with `kubectl exec … psql`. Leave them manual, or a
   typed `sql.run` with a `BEGIN … ROLLBACK` dry-run and row counts on the card?
7. **Passkey-signed approvals.** Worth doing in slice 1 rather than slice 4, given the API process
   could otherwise forge an approval?
8. **Merge approval.** Should `merge_pull_request_with_approval` move onto this mechanism later, so
   there is one approval path, or stay as it is?
9. **Children.** Keep depth-2 tasks unable to request, or let them request with the supervisor as a
   first reviewer?
