# Privileged actions

Status: **proposed**, with the owner's answers applied (see [Decisions](#decisions)). Nothing here
is implemented. It builds on implemented pieces: merge proposals and receipts
([merge policy](../specs/merge-policy.md), [pull requests](../specs/pull-requests.md)), Needs-you
attention and queue cards ([inbox](../specs/inbox.md)), ntfy push
([push notifications](../specs/push-notifications.md)), the `watch_checks` wake ([CI](../specs/ci.md)),
per-binding MCP credentials ([credentials](../specs/credentials.md)) and owner API authentication
([authentication](../specs/authentication.md)).

## Problem

The owner still runs the credentialed steps of agent work by hand: merges and PR closes with `gh`,
1Password lookups with `op`, `kubectl` writes on internal-01, `talosctl` and `omnictl`, and `ssh`
to hosts on the home network (the NAS, a Windows host, the router, a VM on a Mac worker, the UniFi
console). Agents cannot do these, correctly: they hold no credentials, and Claude's auto-mode
classifier holds such commands even where a credential exists. This is the main thing keeping the
owner in a terminal rather than in Mainloop.

Goal: **full infrastructure admin from the phone, as the owner has locally.** An agent
**requests** a privileged action; the owner approves it from the phone, seeing exactly what will
run; an executor that holds the credential runs exactly that; the result goes back to the
requesting session. Agents never hold the credential, and secrets never reach a card or a
transcript. Every action is approved first; nothing bypasses a rule.

## Decisions

The owner answered the open questions of the first draft (2026-11-23). They are binding here:

| Question               | Answer                                                                                                                    |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Bypass of branch rules | **None.** No `bypass_rules`. A merge that rules refuse stays refused.                                                     |
| Second GitHub App      | **No.** Merges and closes use Mainloop's existing App; it needs only pull requests and checks.                            |
| Slice order            | GitHub merge/close, then 1Password, then Kubernetes and the home-network hosts. Passkey-signed approvals later (slice 4). |
| Expiry                 | 24 h undecided; 1 h for deletes.                                                                                          |
| Edit on the card       | No. Deny with a note; the agent requests again.                                                                           |
| Database writes        | Stay manual. No `sql.run`.                                                                                                |
| Children (depth 2)     | Cannot request; they report blocked to their supervisor.                                                                  |
| Merge approval         | `merge_pull_request_with_approval` moves onto this mechanism later, so there is one approval path.                        |
| Scope                  | Full admin: `kubectl`, `talosctl`, `omnictl` and `ssh` to named home-network hosts, through this mechanism (§1b).         |

## 1. Action model

Two kinds of action share one mechanism (request, preflight, card, digest, one-shot execution,
audit):

- **Typed actions** (§1a): a catalog entry per operation with a schema, a semantic preflight, a
  rendered diff, remote preconditions and an `observe` step. Used wherever one exists.
- **Approved commands** (§1b): one exact command line for a named tool on a named target, shown
  verbatim on the card with the output of a read-only preflight. Used for everything else the owner
  does in a terminal today.

Every catalog entry, typed or command, is versioned and compiled into Mainloop and the executor:

| Field        | Meaning                                                                                  |
| ------------ | ---------------------------------------------------------------------------------------- |
| `type`       | Stable name, such as `github.pr_merge`, `k8s.rollout_restart`, `command.ssh`             |
| `version`    | Bumped when arguments, preflight, rendering or execution change                          |
| `args`       | JSON Schema; validated at the MCP boundary, canonicalized (defaults filled, keys sorted) |
| `preflight`  | Read-only step run before the card exists; returns frozen facts (see §2)                 |
| `render`     | Builds the card from args and frozen facts only; never from agent prose                  |
| `execute`    | The one write, with remote preconditions from the frozen facts where the type has them   |
| `observe`    | Reads remote state to decide whether the write happened (typed actions only; see §5)     |
| `credential` | Which executor credential it uses (see §3)                                               |
| `floor`      | `ask_always` or `auto_allowed` (see §4); a server constant, not owner data               |

### 1a. Typed actions

| Type                               | Args (abridged)                                                   | Preflight freezes                                                          |
| ---------------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `github.pr_merge`                  | repo, pr, expected_head_sha, method                               | head/base SHAs, mergeability, checks, reviews, files changed               |
| `github.pr_close`                  | repo, prs (≤ 20), comment                                         | each PR's title, author, state, head SHA                                   |
| `onepassword.item_to_secret`       | item ref (vault/item/field), target namespace, Secret name, key   | item and field exist (metadata only), target Secret's current owner/labels |
| `k8s.rollout_restart`, `k8s.scale` | cluster, namespace, kind, name, (replicas)                        | object UID, resourceVersion, current replicas, Argo tracking               |
| `k8s.patch`                        | cluster, namespace, kind, name, patch type, patch                 | UID, resourceVersion, server dry-run result and diff                       |
| `k8s.apply`                        | cluster, namespace, manifests (inline, ≤ 64 KiB), force_conflicts | server-side-apply dry-run per object and diff against live, Argo tracking  |
| `k8s.delete`                       | cluster, namespace, kind, name                                    | UID (used as the delete precondition), owner references, finalizers        |
| `argocd.sync`                      | app, prune (default false)                                        | app revision, out-of-sync resources                                        |

`github.pr_merge` never bypasses a rule: the App has no bypass, and a merge that branch rules or
required checks refuse fails with GitHub's reason. Normal merges of Mainloop project repositories
keep using `merge_pull_request` under merge policy until that tool moves onto this mechanism;
`github.pr_merge` covers what merge policy does not (repositories that are not Mainloop projects,
such as the infrastructure repository, and merges the owner wants to see first).

### 1b. Approved commands

For the rest of the owner's terminal work, an approved command runs **one exact argument vector**
of one allowlisted tool against one named target:

| Type               | Tool                | Target                                  | Credential (§3)                               |
| ------------------ | ------------------- | --------------------------------------- | --------------------------------------------- |
| `command.kubectl`  | `kubectl`           | a cluster name (`internal-01`)          | a 10-minute token for `executor-admin`        |
| `command.talosctl` | `talosctl`          | an Omni cluster, and nodes in `--nodes` | the Omni service account, through Omni        |
| `command.omnictl`  | `omnictl`           | the Omni instance                       | the executor's Omni service account           |
| `command.ssh`      | `ssh`               | a host name from the host allowlist     | that host's own key, user and pinned host key |
| `http.unifi`       | the UniFi local API | the UniFi console                       | the UniFi API key                             |

```text
args = { target, argv: [string] (≤ 64 items, ≤ 8 KiB), stdin?: string (≤ 64 KiB),
         preflight_argv?: [string], timeout_seconds (≤ 600) }
http.unifi args = { method, path, body? (JSON, ≤ 64 KiB) }
```

**No shell on the executor side.** `argv` is executed directly (`exec`, not `sh -c`): no pipes,
redirection, globbing, variable or command substitution. Flags that read local files or
credentials (`-f <path>`, `--kubeconfig`, `--talosconfig`, `--context`, `-i`, `-F`, `-o
ProxyCommand=…`) are refused; content goes in `stdin` (`kubectl apply -f -`), which the card shows
in full. The executor fills in the target and credential flags itself. For `ssh` the remote side
still runs the command string through the remote user's shell (that is how `ssh` works); the card
shows the exact string the remote shell receives and is tagged `remote shell`. Windows hosts get
the string as their OpenSSH default shell receives it. As built, `command.ssh` takes that string as
`command` (one line) plus `stdin`, not an argv: the executor's own client sends it as the exec
request, with no local ssh, so there are no ssh flags to refuse.

**Preflight.** Read-only, run before the card exists, by the same tool with the same credential
where the tool can stay read-only, its output frozen into the facts (bounded to 16 KiB on the card,
the full output in the audit):

| Type               | Preflight                                                                                                                                                                                                                                                                  |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `command.kubectl`  | Derived: `kubectl diff` for `apply`/`patch`/`replace`/`set`; the same command with `--dry-run=server` for `create`/`delete`/`scale`/`label`/`annotate`; `kubectl get -o yaml` of the named objects otherwise (Secrets refused, below). Argo tracking of each named object. |
| `command.talosctl` | As built: none. Reads need none; `apply-config` and `patch` are left out (their preview reads the machine config); the other changes have no dry run, and the card says so.                                                                                                |
| `command.omnictl`  | `--dry-run` for `apply` and `cluster delete`, through the executor's proxy forwarding reads only; none for reads and `delete`.                                                                                                                                             |
| `command.ssh`      | As built: the host key matches the pinned one and the login succeeds; nothing runs. No requester preflight command: run through the same admin credential, "read-only" would be only the requester's claim, so a read is its own approved `command.ssh`.                   |
| `http.unifi`       | `GET` of the same path.                                                                                                                                                                                                                                                    |

**Binding.** The digest covers type, target, argv, stdin, preflight argv and the digest of the
preflight output. Immediately before executing, the executor re-runs the preflight and compares
the output digest with the frozen one; a difference fails the action as `stale` (deterministic
preflights such as `kubectl diff` make this a real check; for `ssh` without a preflight it is only
the host key, user and address). The agent then requests again and the owner sees the new state.

**What approved commands give up compared with typed actions**, stated plainly:

- **No semantic preconditions.** A typed action pins a resourceVersion, UID or head SHA and the
  remote refuses a changed object. An approved command only has the re-run preflight digest; a
  change the preflight does not print goes unnoticed.
- **No `observe`, no retry.** After a timeout or crash the outcome is `uncertain` and stays so; it
  becomes an owner attention item and is never re-run.
- **No result schema.** The output is free text (bounded and redacted, below), not typed fields.
- **Review is the owner reading a command.** The card shows exactly what runs, but its meaning is
  only as clear as the command; `ssh` commands are interpreted by a remote shell the executor does
  not control.
- **Least privilege inside the target is gone.** `executor-admin` is cluster-admin for one command;
  a host key is that host's admin user. The guard is the approval, not the credential.

**Guards that remain:**

- **Allowlists.** Only the five types above, only allowlisted tools, only named targets from the
  host allowlist (§3); an unknown host, IP literal, or `ssh` jump/forwarding flag is refused.
- **One credential per target.** Each command runs with exactly the credential of its target; no
  command can use another host's key, and no credential is in the agent's workspace.
- **No secrets on the card.** Argv and stdin go through the session redactor before the request is
  accepted; a match refuses the request (`contains_secret`) rather than masking it, so the card
  always shows the real command. Commands that print secrets are refused: `kubectl get|describe
secret(s)`, `kubectl … -o yaml` on Secrets, `talosctl get machineconfig`, `talosctl config`,
  `talosctl gen`, `omnictl talosconfig|kubeconfig|serviceaccount create`, and `op` anywhere. This
  list is best-effort, so output redaction below still applies.
- **Output redaction.** The executor removes every exact occurrence of each credential it holds
  for the run (it knows them; a pattern redactor would miss a bare value), then applies the session
  redactor and a 64 KiB bound before the output reaches the result, the audit or the session.
- **Audit.** Every request, preflight output digest, decision, execution and output digest is an
  append-only event (§3).
- **Expiry and one-shot.** Undecided after 24 h (1 h for deletes and any command tagged
  `destructive`) the request expires; approved and unclaimed for 5 minutes it expires; one
  approval runs once.
- **Hard floor.** Every approved command is `ask_always` (§4). No auto rule and no "allow this
  target for 24 h" ever applies to one.
- **Self-protection.** Commands that change the executor itself are refused: the
  `mainloop-executor` namespace, its RBAC, its Talos and Omni service accounts, and the host
  allowlist. Those change through GitOps the owner merges.

Requests outside both kinds (another tool, an unknown host, `sudo` on the owner's Mac) fail with
`unsupported_action`; the agent reports blocked with the exact command and the owner runs it as
today. A command that keeps being requested is the signal to add a typed action.

## 2. Approval binding

### Request

```text
request_privileged_action(type, args, reason, request_id) ->
  { action_id, state, expires_at, summary }
```

Mainloop takes the requester's identity (binding, session, task, role, depth) from MCP
authentication, never from arguments. It validates and canonicalizes `args`, writes the intent row
(§5), and returns. `request_id` makes the call idempotent per session, as in `watch_checks`.
`reason` is agent text: stored, shown labelled as the agent's claim, and excluded from the
digest's meaning.

Preflight then runs and stores the **frozen facts**. The request becomes a **proposal** with an
immutable digest:

```text
digest = SHA-256(canonical JSON {
  type, version, args, frozen_facts, requester binding/session/task,
  action_id, expires_at, catalog version })
```

The same canonical JSON rules as merge proposals apply (UTF-8, sorted keys, compact, no NaN).

### What the owner sees

On `ask` (§4), Mainloop opens a Needs-you approval card (queue item `privileged_action_<id>`) linked
to the requesting task, in the same transaction as the proposal, and the existing ntfy sweep pushes
it. The push opens the card; the card is the only place to approve. ntfy action buttons are not
used: an approval must come through the authenticated owner API carrying the card's digest.

The card is rendered by the server from args and frozen facts:

- a one-line title from the catalog (`Merge oldsj/example#12`, `Run on nas: systemctl restart
smbd`), never the agent's words;
- the exact target and what will change: the diff for `k8s.*`, commits, files and checks for
  merges, the verbatim argv, stdin and preflight output for approved commands;
- risk tags: `destructive`, `writes a Secret`, `Argo-managed: will drift or be reverted`,
  `irreversible`, `cluster-admin`, `remote shell`, `touches Mainloop's control plane`;
- the requester chain (task, supervisor, main) and the agent's reason, set apart and labelled;
- expiry.

Answers: **Approve**, or **Deny** with an optional note. Approve sends the digest as
`metadata.reviewed_context`; a missing or different digest is recorded as a denial ("the approval
did not carry this card's digest"), never as an approval. Any answer other than Approve is a
denial, and its text is the note delivered to the agent.

### Binding rules

- **Exact.** The decision names the digest. The executor recomputes the digest from the stored
  canonical args and facts before running and refuses a mismatch.
- **One-shot.** One approval is claimed by one execution. A retry after an uncertain outcome is the
  same execution (§5), never a second claim.
- **Preconditions.** Typed actions carry frozen facts as remote preconditions (`sha` for merge,
  `resourceVersion`, UID); approved commands re-run their preflight (§1b). Changed state fails as
  `stale`; nothing is re-planned silently.
- **Expiry.** 24 hours undecided (1 hour for deletes and destructive commands); 5 minutes approved
  but unclaimed.
- **Deny.** Recorded with the owner's note, which is delivered to the agent. No card edits: an
  edited action is a new request with a new preflight and digest.

### What the agent sees while waiting

The `watch_checks` pattern: the tool returns `pending_approval`, the agent ends its turn, and its
workspace may sleep. When the action reaches a final state (succeeded, failed, stale, denied,
expired, uncertain), Mainloop sends the session one message, idempotent on the action id, with the
outcome and the result. `get_privileged_action(action_id)` gives a snapshot;
`cancel_privileged_action` withdraws an undecided request.

### Why not kagent `requireApproval`

- A deferred tool call keeps the turn's delivery open, and later messages queue behind it. For main
  that means the owner cannot talk to main while an approval is pending.
- Binding consent to a native tool call is the costly part of merge policy (template/provider
  mapping, alias verification, receipt lookup).
- Every requesting Agent would need the protected tool server in its template.
- The card needs server-resolved facts (diff, head SHA, preflight output), so Mainloop builds a
  proposal either way.

The ops agent requests writes through this mechanism, and its tool server stays read-only.
`merge_pull_request_with_approval` moves onto it later.

## 3. Executor

### Where it runs

In the cluster, never on the owner's Mac: no relay on the Mac and no new public route.

- **Slice 1 (GitHub).** The executor loop runs inside `serve-api` as a reconcile loop. Merges and
  closes use Mainloop's existing GitHub App, whose key that process already holds and already
  merges with under merge policy, so a separate process would protect nothing yet.
- **From slice 2.** `mainloop serve-executor`, its own Deployment in its own namespace
  (`mainloop-executor`) on internal-01, deployed through GitOps, with the first credential the API
  process must not hold (1Password). It has **no ingress** (no Service; a NetworkPolicy denies all
  ingress): it takes work from PostgreSQL by polling every 3 seconds and writes results back. No
  workspace, Session or MCP server can reach it. The executor's tools (`kubectl`, `talosctl`,
  `omnictl`) are pinned by version and checksum in its image.
- **Network reach.** Two paths, because pod traffic leaving the cluster is masqueraded to the
  node's address, so the router sees the whole cluster zone and cannot tell the executor apart.
  - _Pod network_ (from the node's address): PostgreSQL, the Kubernetes API, `api.github.com`,
    the 1Password API and the Omni endpoint. A CiliumNetworkPolicy in the infrastructure repository
    allows only these.
  - _Its own source address_ for home-network hosts (`ssh`, the UniFi console). This follows Home
    Assistant's VLAN 41 pattern: a Multus macvlan interface `net1` with one static address, pinned
    to the worker that has the VLAN link, with routes only to the allowlisted hosts' subnets. The
    address is a `network.nix` host, and `policy.nix` gets an address zone for it with one rule per
    allowlisted host and port (22 to the NAS, the router, the Windows host and the UTM VM; 443 to
    the UniFi console). The cluster zone gains nothing. Whether the address sits on the cluster
    VLAN or on a small VLAN of its own, and every router rule, is a network change the owner
    approves. Until then the `ssh` and `http.unifi` types answer `unreachable`. The read-side
    infra tools (Omni/Talos reads, `lan_check`) get a separate address zone with read-only ports,
    so reads and writes never share a source identity.

### Host allowlist

As built for `command.ssh`: the chart's `executor.ssh.hosts`, set in the infrastructure
repository's values (addresses are not in this repository). Each entry is a name and an IP address
with its port; the remote user and the pinned host-key line are in that host's 1Password item with
its key (§3), so the owner sets all three together. Illustrative names: `nas`, `router`,
`windows-host`, `utm-vm`. Adding a host is a reviewed GitOps change that restarts the executor; it
refuses targets not on the list. `http.unifi` has one console, `executor.unifi.address`, set the
same way.

### Credentials

Least privilege per type, each mounted only into the executor (from slice 2) and used only for its
type. 1Password items live in the vault the Connect operator already syncs and reach the executor
as Secrets through `OnePasswordItem` resources in the infrastructure repository. **The owner
creates every item; agents never create or read them.**

| Credential           | Source                                                                                                                                                                                                                                                                                                                                                                                                                                          | Used by                    | Slice |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------- | ----- |
| GitHub               | Mainloop's existing GitHub App (pull requests and checks permissions); tokens minted per call for one repository                                                                                                                                                                                                                                                                                                                                | `github.*`                 | 1     |
| 1Password            | Item `mainloop-executor-1password`, field `token`: a 1Password Service Account, **read-only**, scoped to named vaults (not the Connect operator's token)                                                                                                                                                                                                                                                                                        | `onepassword.*`            | 2     |
| Kubernetes, typed    | No item. The executor's ServiceAccount may only `create serviceaccounts/token` for narrow ServiceAccounts (`executor-workloads`, `executor-delete`, `executor-secret-writer`, `executor-argo`); each run mints a 10-minute token                                                                                                                                                                                                                | `k8s.*`, `argocd.sync`     | 3     |
| Kubernetes, commands | No item. The same token minting for `executor-admin`, bound to `cluster-admin`, used only by `command.kubectl`                                                                                                                                                                                                                                                                                                                                  | `command.kubectl`          | 3     |
| Talos                | Preferred: no item. `talosctl` goes through Omni with a talosconfig from the Omni service account below, so Talos grants follow the Omni role (Operator → `os:operator`; Admin → `os:admin`, to confirm against Omni's docs) and the control plane needs no change. Fallback: a `talos.dev/v1alpha1` ServiceAccount `mainloop-executor-talos` with `os:admin`, which needs a `kubernetesTalosAPIAccess` control-plane change the owner approves | `command.talosctl`         | 3b    |
| Omni                 | Item `mainloop-executor-omni`, field `service-account-key`: an Omni service account, role Admin (the owner's choice; it gives Talos `os:admin`). Write-side only; the read tools use a separate Reader account.                                                                                                                                                                                                                                 | `command.omnictl`          | 3b    |
| SSH, per host        | Item `mainloop-executor-ssh-<host>` per host (`nas`, `router`, `windows-host`, `utm-vm`), fields `private-key` (a new ed25519 key for this executor only, public half installed for that host's admin user), `user` and `known-hosts` (the host's key line)                                                                                                                                                                                     | `command.ssh` to that host | 3b    |
| UniFi                | Item `mainloop-executor-unifi`, fields `api-key` (a UniFi local API key from the console) and `certificate` (the console's PEM certificate, which the executor pins)                                                                                                                                                                                                                                                                            | `http.unifi`               | 3b    |

A host whose item is missing is reported as `credential_missing` with the item and field names;
the executor never falls back to another credential.

As built in slice 3 (first part): the accounts are `mainloop-executor-workloads` (get and patch
Deployments, StatefulSets and DaemonSets; list autoscalers) and `mainloop-executor-delete` (get and
delete any object), with cluster roles named after the executor's namespace. RBAC cannot grant
"everything but Secrets", so the delete account can read Secrets; the executor only ever reads
through it with metadata-only requests, and the guard on its own namespace and roles is in the
executor. `onepassword.item_to_secret` kept slice 2's Role in each allowed namespace instead of a
minted `executor-secret-writer` token. `k8s.patch` and `k8s.apply` use a third account,
`mainloop-executor-writer` (get, create and patch any object; escalate and bind on roles), and
refuse Secrets. Their binding is the dry-run diff: the executor re-runs the dry run immediately
before writing and writes only if the diff is unchanged, then writes at the resourceVersion it just
observed.

### Database and audit

The executor has its own database role, `mainloop_executor`. The database operator creates it,
with a password the owner sets. The role has:

- `SELECT` on the actions and their events
- column `UPDATE` on the preflight and execution columns only
- `INSERT` on the event log

`mainloop migrate up` grants these privileges on every run once the role exists, through
`privileged_executor_grants()`. It cannot write a decision, nor read any other table. The reverse
does not hold (as built in slice 2): the API role still owns the tables and can write every column,
so a compromised API process could forge a decision row. Slice 4's passkey-signed approvals are
what close that.

Results never contain a credential. For `onepassword.item_to_secret` the executor reads the field
into memory, by id, and writes it into the Secret only if the item's version equals the frozen
one. A missing Secret is created. An existing one gets a JSON merge patch of the one key, carrying
the frozen resourceVersion, so a concurrent change fails the write instead of being overwritten.
Both write with field manager `mainloop-executor` and annotation
`mainloop.dev/privileged-action: <id>`. The patch is not server-side apply, because a later apply by
the same manager would drop keys that an earlier action wrote. The result is the Secret's name,
key and resourceVersion, never the value or a hash of it. Error and command output go through the exact-
value removal, the session redactor (`server/internal/sessions/redact.go`) and a length bound.

An append-only `privileged_action_events` table records requested, preflighted (facts digest),
decided (owner principal, decision, digest), claimed, executed (result or output digest), observed
and delivered. Remote objects carry the action id where they can (annotation on Kubernetes objects;
a comment on the PR for merges and closes, naming the approval). An owner page lists actions and
their events (slice 2).

## 4. Policy

Policy decides `auto`, `ask` or `deny` per proposal. It is owner data, versioned with an immutable
audit row per change, like merge policy.

- **Default: everything asks.** No rule auto-approves until the owner writes one (slice 4).
- **Hard floor.** `ask_always` types can never be auto-approved: every delete, `k8s.apply`,
  `k8s.patch`, `onepassword.*`, anything tagged `Argo-managed`, `github.pr_merge`, and **every
  approved command**. The floor is compiled into both the API and the executor; the executor
  refuses an auto decision on a floor type even if the API recorded one.
- **Pre-approvable candidates** (slice 4): `k8s.rollout_restart` and `k8s.scale` (within bounds, not
  to 0) outside the protected namespaces, `argocd.sync` without prune, `github.pr_close` of PRs
  authored by Mainloop's App or Renovate.

**Who can request:**

| Role                                     | Can request                                                    |
| ---------------------------------------- | -------------------------------------------------------------- |
| Main thread                              | Any catalog type                                               |
| Supervisors (depth 1)                    | Any catalog type                                               |
| Ops agent                                | Any catalog type                                               |
| Code and coordination children (depth 2) | Nothing; they report blocked to their supervisor, who requests |

**Limits.** At most 3 undecided requests per session and 10 overall; more fail with
`too_many_pending`. A request whose canonical args match an undecided one from the same session
returns that one.

## 5. Durable state

- `privileged_actions`: the intent. Requester binding/session/task/role, `(session_id, request_id)`
  unique, `type`, `version`, canonical `args`, `reason`, `state`, `expires_at`, `frozen_facts`,
  `digest` (both immutable once written), decision columns, execution columns (`attempts`,
  `claimed_until`, `result`), delivery columns.
- `privileged_action_events`: append-only audit (§3).
- `privileged_action_rules` and its audit: policy (§4, slice 4).

```text
requested -> preflighting -> pending_approval -> approved -> executing -> succeeded
                         \-> preflight_failed  \-> denied           \-> failed | stale
                                               \-> expired          \-> uncertain -> (typed: observed) succeeded | failed
                                               \-> cancelled
```

Delivery to the session is separate (`pending`, `delivered`, `undeliverable`), as for check
watches, with the action id as the message identity.

The reconciler rule:

- **Intent first.** The tool commits the row before anything else; no network I/O in that
  transaction.
- **Claim, commit, then I/O.** The executor claims a row with a lease and commits `executing`
  before calling the remote. No lock or transaction is held across the call. One execution per
  target at a time.
- **Idempotency key.** The action id, carried into the remote where it allows: annotation on
  Kubernetes objects; `kubectl.kubernetes.io/restartedAt` set to the approval time; the merge
  comment; the Secret's annotation.
- **Observe before retry.** After a timeout or crash a typed action's outcome is `uncertain`, and
  the executor runs `observe` (PR merged at the expected head; PR closed; restart annotation equals
  the action's value; object gone or UID changed; Secret annotation names the action). Done counts
  as `succeeded`; not done, with preconditions holding, retries only for `retry_safe` types.
  Approved commands have no `observe`: uncertain stays uncertain and goes to the owner.

## 6. Slices

**Prerequisite.** Owner API authentication in `enforce` (#420, #422 rollout). An approval must be
attributable to the owner. The gate (`MAINLOOP_PRIVILEGED_ACTIONS_ENABLED`, exact `true`) refuses
to start without `MAINLOOP_AUTH_MODE=enforce`. Today both dev and prod run `observe`, so the live
proof of slice 1 waits for `enforce` on dev.

**Slice 1: the mechanism, plus GitHub.** The tables, the three MCP tools
(`request_privileged_action`, `get_privileged_action`, `cancel_privileged_action`), the approval
card with the digest, ntfy push through the existing sweep, the wake message, the executor loop in
`serve-api`, and `github.pr_merge` and `github.pr_close` through Mainloop's App.

**Slice 2: 1Password.** `onepassword.item_to_secret`; `serve-executor` as its own no-ingress
Deployment with its own database role; the audit page.

**Slice 3: Kubernetes.** `k8s.*`, `argocd.sync` and `command.kubectl`, with the token-minting
ServiceAccounts. The ops agent requests writes through these.

**Slice 3b: home-network hosts.** `command.talosctl`, `command.omnictl`, `command.ssh` and
`http.unifi`. As built, the first two went first: both go through Omni with one Admin service
account that only a re-signing proxy in the executor holds (docs/specs/privileged-actions.md), so
they need no network reach but Omni's endpoint and no Talos credential. The proxy lets them name
only an allowlist of resource types, refuses a type its resource definition marks sensitive, holds
omnictl to `executor.omni.clusters` (writes, and what it shows) and talosctl to its target's
machines as Omni lists them, and upgrades only to Sidero's images: a security review found that a
rule over type names alone missed types that hold secrets. A follow-up refused the types and logs
that show the kernel command line, which may hold the SideroLink join token. `command.ssh` came
next, with its own ssh client (Go's `x/crypto/ssh`, not OpenSSH: no client configuration to lock
down, and nothing but the exec request is asked for) and the host allowlist; it reaches a host once
the owner has created its item in §3 and approved the executor's source address and its router
rules (§3, Network reach). `http.unifi` came last, with its own HTTPS client pinned to the console's
certificate, Network application paths only (the console's admins, API keys, site management and
backups stay the owner's), secret fields refused in bodies and redacted in answers, and a `GET` of
the same path as the preview for `PUT`, `PATCH` and `DELETE`.

**Slice 4: policy and stronger approvals.** Owner-written auto rules and "allow this target for
24 h" (never for floor types); passkey-signed approvals, so a compromised API process cannot forge
one; `merge_pull_request_with_approval` onto this mechanism.

**Not planned.** `sudo` on the Mac (the executor cannot reach it), database writes, any bypass of
branch rules.

## 7. Threats

| Threat                                                                                              | Mitigation                                                                                                                                                                                                                                                                          |
| --------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Prompt injection asks for an action** (a PR comment or log line tells the agent to run a command) | The card shows the target, diff or verbatim command from server facts, never agent prose; the reason is labelled as the agent's; children cannot request; commands are always ask. The residual risk is the owner approving a well-disguised command, which the next row addresses. |
| **Approval fatigue**                                                                                | Typed actions where they exist, because a diff reads faster than a command; batched `github.pr_close`; idempotent duplicates; pending limits; later, auto rules for low-risk typed targets so cards are rare. Approved commands always carry their risk tags.                       |
| **A command with a hidden meaning** (`ssh nas 'curl … \| sh'`, a remote alias)                      | Tagged `remote shell`; preflight output shown; output redacted. Accepted residual risk: the owner asked for full admin, and the approval is the guard.                                                                                                                              |
| **Secrets leaking through output**                                                                  | Secret-reading commands refused; exact-value removal of every credential the run holds; session redactor; length bound; secrets never in argv (refused, not masked).                                                                                                                |
| **Replay**                                                                                          | One-shot claim; digest includes the action id and expiry; preconditions or re-run preflight; 5-minute approved-but-unclaimed expiry.                                                                                                                                                |
| **Compromised workspace**                                                                           | It holds only its MCP bearer. It can request (bounded by role and limits), not approve or execute. It cannot reach the executor, which has no ingress.                                                                                                                              |
| **Spoofed approval through the owner API**                                                          | Requires `enforce` owner auth; decisions record the verified principal; the executor rechecks digest and floor.                                                                                                                                                                     |
| **Compromised Mainloop API process**                                                                | From slice 2 it holds no executor credential, but it can write decision rows. Residual until slice 4 makes Approve a WebAuthn assertion over the digest, verified by the executor.                                                                                                  |
| **Executor self-escalation** (an approved command widens the executor's own authority or allowlist) | Commands on the executor's namespace, RBAC, Talos/Omni accounts and host allowlist are refused; those change only through GitOps.                                                                                                                                                   |
| **Argo drift**                                                                                      | Preflight detects Argo tracking; the card says the change will be reverted or will drift; floor ask. GitOps stays the way to make lasting changes.                                                                                                                                  |
