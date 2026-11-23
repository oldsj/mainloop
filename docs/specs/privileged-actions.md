# Privileged actions

An agent asks Mainloop to run a credentialed action it cannot run itself. The owner approves that
exact action from one Needs-you card, which is pushed to their phone. Mainloop then runs it once and
sends the requesting session one message with the outcome.

**Implemented:**

- Slice 1: the mechanism, plus two catalog types, `github.pr_merge` and `github.pr_close`. They run
  through Mainloop's own GitHub App, in `serve-api`.
- Slice 2: `onepassword.item_to_secret`, run by a separate process, `serve-executor`, which holds a
  1Password credential `serve-api` never sees. Also the owner's read-only audit page.
- Slice 3, first part: the typed Kubernetes actions `k8s.rollout_restart`, `k8s.scale` and
  `k8s.delete`, run by `serve-executor` with short-lived credentials it mints per run.

`k8s.patch`, `k8s.apply`, `argocd.sync`, approved commands and the home-network types are later
parts. They are not described here.

## Enablement

Privileged actions are off unless `MAINLOOP_PRIVILEGED_ACTIONS_ENABLED` is exactly `true`. The
chart pins it to `false`. With it on, `serve-api` refuses to start unless `MAINLOOP_AUTH_MODE` is
`enforce`, because an approval has to be attributable to the owner. When privileged actions are off,
or there is no GitHub App, the tools are still listed but answer `[unavailable]`.

The executor types are accepted only when `MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED` is also exactly
`true`. The chart sets it when `executor.enabled` deploys `serve-executor` (see
[The executor](#the-executor)). Without it, a request of an executor type is `[unsupported_action]`.

The `k8s.*` types also need `MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED`, exactly `true`. The
chart sets it with `executor.kubernetes.enabled`, which is off by default.

## Tools

| Tool                        | Who                                                |
| --------------------------- | -------------------------------------------------- |
| `request_privileged_action` | main (depth 0), coordinating supervisors (depth 1) |
| `get_privileged_action`     | the same                                           |
| `cancel_privileged_action`  | the same                                           |

Children, workspace agents and root code tasks cannot request. They report blocked to whoever
supervises them.

`request_privileged_action {request_id, type, args, reason}`:

- `request_id` is the caller's idempotency key, unique per session. Repeating it with the same type
  and arguments reads the same action back (`replayed: true`). Repeating it with different
  arguments is a `[conflict]`.
- A repeat with the same arguments under a new `request_id`, while the first action is undecided,
  also returns the first action.
- At most 3 undecided actions per session, and 10 per owner (`[too_many_pending]`).
- `reason` is shown on the card as the agent's claim, set apart from the facts. It is not part of
  the digest.

The tool returns at once with the action's state. The agent ends its turn. When the action ends,
the session gets one message, whose id is the action id, naming the final state, the owner's note
(if any) and the result. `get_privileged_action` returns the full record, including the audit
events. `cancel_privileged_action` withdraws an undecided action and closes its card.

## Types

`github.pr_merge {repo, pr, expected_head_sha}` squash-merges the pull request at exactly
`expected_head_sha`. The merge method is fixed.

`github.pr_close {repo, prs, comment?}` closes 1 to 20 open pull requests without merging them.
The numbers are sorted and deduplicated. The comment is posted on each pull request, so a comment
that looks like it holds a secret is refused.

Neither type bypasses a branch rule. GitHub's own protection still decides the merge.

`onepassword.item_to_secret {vault, item, field, namespace, secret, key}` copies one field of a
1Password item into one key of a Kubernetes Secret:

- `vault`, `item` and `field` are each an id or an exact title. An ambiguous title is refused:
  name it by id.
- `namespace` must be in the executor's allowlist (`executor.secretNamespaces`). These are always
  refused: `default`, and names starting with `kube-`, `mainloop`, `onepassword` or `argocd`.
- An existing Secret is refused when a controller owns it, Argo CD tracks it, or its type is not
  `Opaque`.
- A field that is empty, or has a type 1Password does not expose, is refused.

The value never leaves the executor: not in the facts, the card, the result, an event or an error.
The 1Password service account is read-only, scoped to the vaults the owner chose.

The Kubernetes types act on the cluster the executor runs in, named on the card from
`executor.kubernetes.clusterName`:

- `k8s.rollout_restart {namespace, kind, name}` restarts a Deployment, StatefulSet or DaemonSet the
  way `kubectl rollout restart` does: it sets the pod template's `kubectl.kubernetes.io/restartedAt`
  to the approval time.
- `k8s.scale {namespace, kind, name, replicas}` sets a Deployment's or StatefulSet's replicas, 0 to 100. Asking for the current count is refused.
- `k8s.delete {namespace, kind, name}` deletes one object. `kind` is a kind or resource name,
  optionally with its API group (`Pod`, `Job.batch`, `Certificate.cert-manager.io`); `namespace` is
  empty for a cluster-scoped object. An object already being deleted is refused. A delete waits
  1 hour for the owner, not 24.

All three refuse the executor's own namespace, and the cluster-scoped objects named after it (its
roles). Those change only through GitOps.

## Preflight and the card

Mainloop canonicalizes the arguments and writes the intent row. It then reads the target with no
transaction open. A target the action cannot apply to ends the request as `preflight_failed`, and
no card opens. That covers a pull request that is missing, closed, merged or draft, or whose head
is not `expected_head_sha`.

A GitHub read that fails for another reason leaves the request `requested`. A sweep retries it, up
to 5 attempts.

On success, the facts are frozen: title, author, head and base, size, mergeability and a count of
checks with up to five failing names. They are frozen under a **digest**, the SHA-256 of the
canonical JSON of:

- the action id
- the type and its version
- the catalog version
- the arguments
- the frozen facts
- the requester (role, session, task)
- the expiry

A database trigger refuses any later change to the digest, facts, type, version or arguments.

The card is a Needs-you `approval` item with high priority, so it gets an ntfy push. It has:

- **Title:** "Merge owner/repo#N", "Close owner/repo#N", "Close K pull requests in owner/repo",
  `Copy 1Password "item › field" to Secret namespace/name`, "Restart Kind namespace/name",
  "Scale Kind namespace/name to N" or "Delete Kind namespace/name", each followed by "on cluster"
  when the cluster has a name.
- **Content:** the rendered facts, risk tags (checks failing, pending or unread; not mergeable;
  overwrites an existing key; a Secret labelled managed-by; destructive, irreversible, scales to
  zero; Argo-managed; an autoscaler or an owner that may undo it; a paused Deployment; a namespace
  that is Mainloop's, the cluster's or Argo CD's),
  the labelled agent reason, the expiry, and the action id with the first 16 characters of the
  digest.
- **Options:** `Approve` and `Deny`. The card also accepts a free-text reply.
- **Context:** `privileged_action_id`, `privileged_action_digest`, `privileged_action_type`,
  `repository`, and `pr_url` for a single pull request.

## Decision

The web app sends `Approve` with `metadata.reviewed_context` set to the card's digest. A sweep
records the decision:

- An **Approve** whose `reviewed_context` equals the digest, sent before expiry, approves the
  action.
- **Any other answer** denies it:
  - `Deny` is a denial with no note.
  - Free text is a denial with that text as the note.
  - An Approve with a missing or different digest, or one sent after expiry, is a denial with a
    note that says so.
  - A card closed without an answer is a denial.

For `onepassword.item_to_secret` the frozen facts are the vault, item and field metadata, with
the item's version, and the target Secret's uid, resourceVersion and key names, or its absence.
For a workload they are its uid, generation, resourceVersion, replicas and their status, images,
last restart, Argo tracking and autoscaler. For a delete they are the resolved kind, the object's
uid, creation time, owners, finalizers and Argo tracking.

An undecided action expires after 24 hours. An approved action that no executor claims within 5
minutes also expires, so a backlog never runs an old approval late.

## Execution

The executor claims an approved action with a 2-minute lease and then:

1. Recomputes the digest from the stored row. If it differs from the approved digest, nothing
   runs and the action fails.
2. Observes the target before writing anything:
   - **Merge:** a head that moved, or a pull request closed meanwhile, makes the action `stale`.
     A merge already made at the frozen head by an earlier attempt of this action counts as
     `succeeded`; on the first attempt it counts as `stale`.
   - **Close:** each pull request is checked on its own. A merged one, or one whose head moved,
     is left as it is.
3. Writes the change.
   - **Merge:** a 409 from GitHub is `stale`. A 405 or another refusal is `failed`. A lost answer,
     a 429 or a 5xx is retried after 30 seconds, up to 5 attempts, and each retry observes first.
     After the last attempt the action is `uncertain`, and the owner gets an error item.
   - **Close:** the result lists each pull request as `closed`, `already_closed`, `merged`,
     `stale` or `failed`. The action succeeds only if every pull request ended closed.
4. Posts a best-effort comment on each pull request it changed, naming the action, plus the
   digest prefix for a merge.

For `onepassword.item_to_secret` (in `serve-executor`):

- **Observe.** A Secret annotated `mainloop.dev/privileged-action: <this action's id>` was
  written by an earlier attempt, so the action `succeeded`. Otherwise the action is `stale` when
  the Secret was deleted, changed (uid or resourceVersion) or created since preflight.
- **Read the value.** The executor reads it by id. An item whose version differs from the frozen
  one is `stale`.
- **Write.** A missing Secret is created with this one key. An existing Secret gets a merge patch
  of this one key, carrying the frozen resourceVersion, so a concurrent change is refused (`stale`)
  and nothing is written. Other keys are left as they are. The field manager is
  `mainloop-executor`.
- **Failures.** A 403 is `failed`. Other errors are retried like a merge, and each retry observes
  first.

For `k8s.rollout_restart` and `k8s.scale` (in `serve-executor`):

- **Observe.** A workload annotated `mainloop.dev/privileged-action: <this action's id>` was written
  by an earlier attempt, so the action `succeeded`. One deleted, replaced (another uid) or with a
  changed spec (another generation) is `stale`. A status change alone does not count.
- **Write.** A JSON merge patch of the restart annotation or `spec.replicas`, plus the action
  annotation, carrying the resourceVersion just observed. A concurrent write is retried, observing
  first.

For `k8s.delete` (in `serve-executor`):

- **Observe.** On the first attempt, an object already gone or replaced is `stale`: someone else
  acted. On a later attempt it is this action's own delete, whose answer was lost, so `succeeded`.
- **Write.** A delete with the frozen uid as its precondition and background propagation, as
  `kubectl delete` does. A replaced object is `stale` and nothing is deleted. The result says when
  finalizers still hold the object.

A 403 from Kubernetes is `failed`; other errors are retried like a merge.

States: `requested`, `pending_approval`, `preflight_failed`, `approved`, `executing`, `succeeded`,
`failed`, `stale`, `denied`, `expired`, `cancelled`, `uncertain`.

Every step appends a row to `privileged_action_events`, which cannot be updated.

## The executor

`mainloop serve-executor` runs the executor types. It has no listener, and its namespace denies all
ingress. It runs one replica, replaced rather than rolled. It polls PostgreSQL every 3 seconds as
its own database role, `mainloop_executor`, which the database operator creates with a password
the owner sets. `mainloop migrate up` grants that role its privileges once the role exists. The
role can:

- read `privileged_actions` and `privileged_action_events`
- write the preflight and execution columns
- append events

It cannot record or change a decision, change what was asked, or read any other table. The
executor refuses to start under a role that can read `attention_items`.

**Kubernetes credentials.** With `executor.kubernetes.enabled`, the chart renders two
ServiceAccounts in the executor's namespace. No pod runs as either:

- `mainloop-executor-workloads`: get and patch Deployments, StatefulSets and DaemonSets, and list
  HorizontalPodAutoscalers, cluster-wide
- `mainloop-executor-delete`: get and delete any object, cluster-wide

The executor's own account may only create tokens for those two. Each run of a `k8s.*` type mints a
10-minute token for the one it needs. The delete account can read every object, Secrets included,
because RBAC cannot grant "everything but Secrets". The executor reads through it with
metadata-only requests, so no object's data reaches it. The guard on the executor's own namespace
and roles is the executor's, not RBAC's.

The split of work:

- **`serve-executor`** preflights an executor type and freezes its facts without opening a card.
  It also claims and runs the approved action.
- **`serve-api`** opens the card, records the decision, expires, delivers the outcome, and raises
  an `uncertain` action to the owner.

An executor type that nobody preflights within 10 minutes ends `preflight_failed`, with a note
that the executor may be down.

`serve-api`'s own database role can still write every column. A compromised `serve-api` could
therefore forge an approval for an executor type. Binding approvals to the owner's passkey (a
later slice) closes that gap.

## Audit

`GET /privileged-actions` lists the owner's 100 newest actions: type, state, the card's title, the
requester, the reason, the times, the note and the error. `GET /privileged-actions/{action_id}`
adds the arguments, the frozen facts, the digest, the card's text, the result and every event. The
web app shows them at `/privileged`, reachable from the command palette. The page is read-only:
the owner approves or denies from the Needs-you card. It reads the table directly, so the history
stays readable when privileged actions are off.

## Delivery

The outcome message is sent at most once, with the action id as the message id.

- A busy session is retried after 3 seconds.
- A session that is gone marks the delivery `undeliverable`.
- Other errors are retried after 30 seconds, up to 20 tries.
