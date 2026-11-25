# Privileged actions

An agent asks Mainloop to run a credentialed action it cannot run itself. The owner approves that
exact action from one Needs-you card, which is pushed to their phone. Mainloop then runs it once and
sends the requesting session one message with the outcome.

**Implemented:**

- Slice 1: the mechanism, plus two catalog types, `github.pr_merge` and `github.pr_close`. They run
  through Mainloop's own GitHub App, in `serve-api`.
- Slice 2: `onepassword.item_to_secret`, run by a separate process, `serve-executor`, which holds a
  1Password credential `serve-api` never sees. Also the owner's read-only audit page.
- Slice 3, first part: the typed Kubernetes actions `k8s.rollout_restart`, `k8s.scale`,
  `k8s.delete`, `k8s.patch` and `k8s.apply`, `argocd.sync`, and the approved command
  `command.kubectl`, run by `serve-executor` with short-lived credentials it mints per run.
- Slice 3, second part: the approved commands `command.omnictl` and `command.talosctl`, run by
  `serve-executor` through a proxy of its own that holds its Omni service account.
- Slice 3, third part: the approved command `command.ssh` to a named home-network host, run by
  `serve-executor` with its own key for that host.
- Slice 3, fourth part: `http.unifi`, one request to the UniFi console's Network application, sent
  by `serve-executor` with its own API key to the console's pinned certificate.
- [Signed approvals](#signed-approvals): Approve from the iPhone app, signed with the phone's Touch
  ID approval key, checked by `serve-api` and again by `serve-executor` against keys pinned in its
  own configuration. Tested with software keys; not yet proven on a phone.

## Enablement

Privileged actions are off unless `MAINLOOP_PRIVILEGED_ACTIONS_ENABLED` is exactly `true`. The
chart pins it to `false`. With it on, `serve-api` refuses to start unless `MAINLOOP_AUTH_MODE` is
`enforce`, because an approval has to be attributable to the owner. When privileged actions are off,
or there is no GitHub App, the tools are still listed but answer `[unavailable]`.

The executor types are accepted only when `MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED` is also exactly
`true`. The chart sets it when `executor.enabled` deploys `serve-executor` (see
[The executor](#the-executor)). Without it, a request of an executor type is `[unsupported_action]`.

The `k8s.*` types, `argocd.sync` and `command.kubectl` also need
`MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED`, exactly `true`. The chart sets it with
`executor.kubernetes.enabled`, which is off by default.

`command.omnictl` and `command.talosctl` need `MAINLOOP_PRIVILEGED_EXECUTOR_OMNI_ENABLED`, exactly
`true`; the chart sets it with `executor.omni.enabled`, which is off by default. Without it they are
`[unavailable]`.

`command.ssh` needs `MAINLOOP_PRIVILEGED_EXECUTOR_SSH_ENABLED`, exactly `true`; the chart sets it
with `executor.ssh.enabled`, which is off by default. Without it, it is `[unavailable]`.

`http.unifi` needs `MAINLOOP_PRIVILEGED_EXECUTOR_UNIFI_ENABLED`, exactly `true`; the chart sets it
with `executor.unifi.enabled`, which is off by default. Without it, it is `[unavailable]`.

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
- A session whose requests of the executor's types (`onepassword.*`, `k8s.*`, `argocd.*`, `command.*`) a
  preflight refused 5 times in the last hour gets `[too_many_refused]` for those types until the
  hour passes. GitHub merges and closes are neither counted nor limited. A refusal opens no card, and whether a request is refused, and why, says
  something about the target, so a session must not probe it unseen. A request refused before any
  read (`[validation]`) does not count.
- `reason` is shown on the card as the agent's claim, set apart from the facts. It is not part of
  the digest.

The tool returns at once with the action's state. The agent ends its turn. When the action ends,
the session gets one message, whose id is the action id, naming the final state, the owner's note
(if any) and the result. `get_privileged_action` returns the full record, including the audit
events, except the diffs of `k8s.patch`, `k8s.apply` and `command.kubectl`, and the dry runs of `command.omnictl`: they hold the objects' contents, read with
the executor's credential before anyone approved, so the agent gets each object's name and whether
it is created, not the diff, its size or a hash of it (any of which would confirm a guess), and only
the owner sees the diff. `cancel_privileged_action` withdraws an undecided action and closes its card.

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
  refused: `default`, and names starting with `kube-`, `mainloop`, `onepassword` or `argocd`. The
  chart refuses to render an allowlist that holds the executor's or the release's namespace.
- An existing Secret is refused when a controller owns it, Argo CD tracks it, it is immutable, or
  its type is not `Opaque`.
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

- `k8s.patch {namespace, kind, name, patch_type, patch}` patches one object as `kubectl patch`
  does. `patch_type` is `merge`, `strategic` or `json`; `patch` is an object, or for `json` an array
  of operations. A patch may not set what the diff does not show or the executor binds: `status`,
  the metadata the API server keeps (resourceVersion, uid, generation, creation and deletion
  times, managed fields, selfLink) or the `mainloop.dev/privileged-action` annotation. A JSON patch
  may not replace `/metadata` whole or use `test` (refused or not, a test would confirm a guess
  about the object without an approval), and a merge patch may not null `metadata` or
  `metadata.annotations` (remove annotations one by one).
- `k8s.apply {manifests, namespace, force_conflicts}` server-side applies YAML or JSON manifests
  (up to 50 objects, 64 KiB) as `mainloop-executor`, as `kubectl apply --server-side` does.
  `namespace` is the default for namespaced objects that name none; an object naming another is
  invalid. Each object needs a name (no `generateName`), and a `List` is given object by object.
  Without `force_conflicts`, fields another manager owns are refused with the conflicts. A manifest
  is held to the same fields as a patch; an empty `status` and a null `creationTimestamp`, as
  generated YAML carries, are allowed.

Preflight runs the patch or apply as a server dry run, and the card shows each object's diff: its
YAML, live against the dry run's answer, without status and the metadata the API server keeps
(resourceVersion, generation, uid, creation time, managed fields). A change that alters nothing is
refused, and an apply skips the objects it would leave as they are. The card shows every diff whole:
diffs over 16 KiB together are refused, never cut.

A patch or manifest that sets what looks like a credential is invalid before any read of the
cluster (`contains_secret`), naming the field. Checked only after the dry run, a credential-named
field set to a guess would end as "changes nothing" when the guess is right and `contains_secret`
when it is wrong, with no card either way. A diff that shows what looks like a credential is refused
too, naming the field's path but not its value, or only the object when the value is folded or
escaped in the diff's YAML. What looks like a credential is a value holding a shape the session
redactor knows (a private key block, a bearer token, a JWT, a known token prefix, credentials in a
URL, an assignment such as `password: …`, also inside one-line JSON), or a `data` or `stringData`
value that is one long word of high entropy, as a random key is. Long names, checksums and CA
bundles pass. Neither this refusal nor the size refusal gives a figure an agent could compare
between guesses.

The dry run runs twice; a value on which the answers differ is one the API server generates on each
write (a Job's controller-uid, a Pod's token volume name, a webhook's timestamp), and the diff and
the check before the write show it as `<server-generated>`, with the card naming its paths. A value
the request itself sets is never masked: if it differs between the runs, the request is refused: the API server generates it, so the request must leave it out.

Both refuse Secrets, since a dry run's answer would carry the value. Copy one from 1Password with
`onepassword.item_to_secret`.

All five refuse the executor's own namespace, and the cluster-scoped objects named after it (its
roles). Those change only through GitOps.

- `argocd.sync {app, namespace, prune}` starts an Argo CD sync of one Application (`namespace`
  defaults to `argocd`) at the revision Argo CD has resolved, as `argocd app sync --revision` does.
  It pins every source's revision for a multi-source Application, and carries the Application's own
  sync options and retry policy (`spec.syncPolicy`), as the argocd server does: Argo CD reads them
  from the operation, not the spec. With `prune`, resources no longer
  in Git are deleted, so the request waits 1 hour for the owner, as a delete does.
  - Refused: an Application that is missing, has a sync pending or running, or has no resolved
    revision yet; an Application in the executor's namespace; and one that deploys into the
    executor: its destination is the executor's namespace, or among the resources Argo CD manages
    for it (`status.resources`) is one in that namespace, that Namespace, or a cluster-wide object
    named after it (`<namespace>-…`). The Mainloop chart's Application is such a one: it deploys
    the release namespace and names the executor's namespace per object. The executor changes only
    through GitOps the owner merges.
  - The card shows the revision(s), sync and health status, the destination, the namespaces of its
    resources, its source(s) (repository, path or chart, target revision, and whether it has inline
    Helm values), its sync options and retry limit, and the resources out of sync or to prune (the
    first 50 by name, with a count). Tags: destructive when it prunes or has `Force=true`,
    `Replace=true`, auto-sync on, and Mainloop's, system or Argo CD namespaces. The agent's view of
    the facts leaves out the sources: inline Helm values can hold anything.
  - The sync's own outcome is Argo CD's: the action succeeds once the sync is started.

`command.kubectl {target, argv, stdin, timeout_seconds}` runs one kubectl command as
cluster-admin, for the work on the cluster that no typed action covers. Prefer a typed action where
one fits: it observes its target and retries, and this does neither.

- `target` is the executor's cluster (`executor.kubernetes.clusterName`); another is refused.
  `argv` is the command's words, starting with `kubectl` and then its verb, run as is without a
  shell: at most 64 words and 8 KiB. `stdin` is what `-f -` or `exec -i` reads, up to 64 KiB.
  `timeout_seconds` is 1 to 600, default 60.
- kubectl never holds the cluster-admin token. The executor mints one for
  `mainloop-executor-admin` that outlives the deadline and keeps it in a proxy of its own on
  loopback, for the one run. kubectl's kubeconfig, in a fresh directory, names only that proxy (TLS
  with a certificate made for the run) and a token made for the run, which the proxy checks and
  swaps for the real one. A flag or kubeconfig that sent kubectl elsewhere would send nothing of
  value. kubectl, at the version the image pins, runs with only `PATH`, `HOME`, `KUBECONFIG`,
  `KUBERC=off` and plugins off. Both tokens are removed from the output (plain and base64), the
  session redactor runs over it, and each stream is kept to 64 KiB.
- The proxy refuses, for every command, approved or not, with `refused by the executor`:
  impersonation headers; Secrets; anything in the executor's namespace, and that Namespace;
  `serviceaccounts/token` and the `proxy` subresources; writes to `admissionregistration.k8s.io`;
  writes to roles and bindings named `<namespace>-…`, and deleting every role or binding of a
  kind; deleting every Namespace; streams but `exec`; writes outside the resource API; and paths
  with escapes or dot segments.
- Before approval the command runs only as the preview's server dry run, and the proxy makes sure
  of that whatever kubectl sends: every create, update and patch gets `dryRun=All`, a DELETE's or
  an eviction's options are rewritten with it, a body it cannot read is refused, and so are
  streams.
- Known limits:
  - A read across every namespace (`-A`) lists the executor's namespace too. The proxy refuses
    paths that name that namespace, not cluster-wide lists, so the executor's objects other than
    Secrets (its pods, ConfigMaps, events) can reach the output of an approved read. Secrets are
    refused everywhere.
  - An aggregated API (an APIService served by a server other than the API server) gets the same
    `dryRun=All`, but honoring it is up to that server. One that ignores it would apply a preflight
    write before the owner approves. Built-in resources and custom resources honor it.
- Refused when requested (`[validation]`): argv is read by kubectl's own parser, from the
  `k8s.io/kubectl` module of the release the image pins, so it reads every word as the binary
  will: an underscore spelling, a value taken from the next word, a short-flag cluster. Then an
  allowlist holds: the commands `get`, `describe`, `logs`, `top pod|node`, `events`, `explain`,
  `api-resources`, `api-versions`, `version`, `cluster-info`, `wait`, `auth can-i|whoami|reconcile`,
  `apply`, `create` (from manifests, or a generator: deployment, job, cronjob, configmap, namespace,
  serviceaccount, role, clusterrole, rolebinding, clusterrolebinding, quota, priorityclass,
  poddisruptionbudget, ingress, service), `replace`, `patch`, `label`, `annotate`, `scale`,
  `set image|env|resources|selector|serviceaccount|subject`, `autoscale`, `expose`, `run`, `taint`,
  `rollout restart|undo|pause|resume|status|history`, `delete`, `drain`, `cordon`, `uncordon` and
  `exec` (whose command follows `--`), each with its own list of flags; every other command and
  flag is refused. No list holds a flag that chooses the target, identity or credential, reads or
  writes a local file (`-f` takes only `-`), `--raw`, `--dry-run`, `--prune`, a watch (but
  `rollout status`'s, which ends), `-A` or `--all` on a write. Also refused: a flag value that starts with `-`; words after `--` but for
  `exec`, `run` and `create`; control and bidirectional characters; `get`, `describe`, `events` or
  `wait` of Secrets, `create secret` and `create token`; a write naming admission policies or
  webhooks, in argv or as a stdin manifest. Argv or stdin that looks like a credential is refused
  (`contains_secret`), as is a manifest in stdin that sets one.
- Preflight refuses a command that names the executor's namespace anywhere in argv or stdin, then
  previews it by its verb:
  - A change (`apply`, `create`, `replace`, `patch`, `label`, `annotate`, `scale`, `set`,
    `autoscale`, `expose`, `run`, `taint`, `rollout restart|undo|pause|resume`, `auth reconcile`)
    runs twice as a server dry run with `-o json`. Each object it prints is diffed against the live
    one as for `k8s.apply`, with values the two runs disagree on masked, and held to the same
    checks: no Secrets, not the executor's own, no credential in a diff, 16 KiB of diffs at most,
    not a change that alters nothing.
  - `delete`, `drain`, `cordon` and `uncordon` run once as a server dry run, and the card shows what
    kubectl printed, 16 KiB at most. A dry run that reaches the executor's namespace or objects is
    refused.
  - A read has nothing to preview. `exec` cannot be previewed: the card says it runs inside a
    container and can do anything that container can.
  - A dry run that fails is refused with its exit code and the error's reason (`NotFound`,
    `Forbidden`), not kubectl's message: that is cluster state read before anyone approved. The
    executor's log keeps the message.
- The card shows the exact command, the stdin, the preview, and tags: destructive (a delete, a
  drain, `--force`, `--grace-period=0`, scaling to zero), the namespaces it names, every namespace,
  runs inside a container, and always cluster-admin, plus the change tags of `k8s.apply`. A command
  that deletes or evicts waits 1 hour for the owner.
- The agent's view of the facts has the preview kind and the changed objects' names, not the diffs
  or the dry run's text.

`command.omnictl {argv, stdin, timeout_seconds}` and `command.talosctl {target, argv,
timeout_seconds}` run one omnictl or talosctl command through Omni as the executor's Omni service
account, whose role is Admin: for Omni resources (clusters, machine sets) and for Talos nodes
(reboots, upgrades, etcd).

- `argv` starts with `omnictl` or `talosctl` and then its command, run as is without a shell: at
  most 64 words and 8 KiB. `stdin` is what `omnictl apply --file=-` reads, up to 64 KiB.
  `timeout_seconds` is 1 to 600, default 60. talosctl's `target` is an Omni cluster from
  `executor.omni.clusters`; another is refused at preflight. omnictl takes no target: it acts on
  Omni, held to those same clusters (below).
- Neither tool holds the service account's key. For each run the executor makes a key of its own
  and a proxy on loopback (TLS with a certificate made for the run) that checks each call is signed
  by that key, then signs it again with the real one and forwards it to Omni with only the routing
  headers. The tool gets the run's key, a configuration naming only the proxy, a CA file holding
  only the proxy's certificate, and `PATH`, `HOME` and its config paths, in a fresh directory. A
  flag or configuration that sent it elsewhere would send nothing Omni accepts. The run's key is
  removed from the output, the session redactor runs over it, and each stream is kept to 64 KiB.
- The proxy refuses, for every command, with `refused by the executor`:
  - any method but the reads and writes the allowed commands use (no kubeconfig, talosconfig,
    service account, support bundle, file read, client certificate, config apply or etcd snapshot;
    no kernel log, through Talos's dmesg or Omni's machine logs, which prints the kernel command
    line where Omni's installation media put the SideroLink join token);
  - a resource type, named in a request or in a resource it writes, that is not on the executor's
    allowlist of types an operator reads: Omni's clusters, machine sets, machines, cluster
    machine statuses, upgrade and etcd backup statuses; Talos's members, addresses, routes, links, disks,
    volumes, services, etcd members, node and hardware statuses. Nothing that holds a credential is
    on it: not Omni's config patches, configs, Siderolink config, join tokens, kubeconfigs,
    talosconfigs or secrets, nor what holds a kernel command line or a machine config diff (Omni's
    machine statuses, kernel args and their statuses, pending updates), nor Talos's machine config, link specs, registry config, manifests,
    KubeSpan and discovery configs, volume configs, extension service configs or Siderolink config;
  - a type whose resource definition, read from Omni or the node with the executor's key, marks it
    sensitive, or whose definition cannot be read; and, as a second check, a type whose name
    matches the secret-bearing words (secret, token, key, identity, config…) or the `secrets`
    namespace;
  - a request that repeats or retypes the fields naming them; an `authorization` header; Talos
    routing headers on an omnictl run;
  - a talosctl call to Omni itself, for another cluster than the target, with no node, or for a
    node that is not a machine of the target in Omni (by address, node name or machine ID, from
    Omni's ClusterMachineIdentities, read once per run): Omni routes a node given by address, so
    the cluster header alone does not hold a request to the cluster. talosctl writes no resource;
  - an omnictl write (create, update, destroy, teardown) of a resource that does not belong to one
    of `executor.omni.clusters`, by its `omni.sidero.dev/cluster` label, or by its ID for the types
    named by their cluster: the proxy reads the resource it replaces or destroys from Omni first.
    A resource of no cluster (a machine class, a machine's labels) is refused: no global change;
  - in omnictl's answers, any resource of another cluster: a list or watch leaves it out, and a
    get is refused. Omni may manage clusters the executor does not act on, and omnictl sees none
    of theirs;
  - `talosctl upgrade` to an image that is not the node's own installer: the Image Factory's for
    the schematic Omni records for that machine (`factory.talos.dev/<platform>-installer/<ID>`),
    or `ghcr.io/siderolabs/installer` for a machine with no customization, tagged with a Talos
    version no older than the one the machine runs, and with no digest (a digest installs whatever
    it names, whatever the tag says). So no extensions or kernel arguments the
    machine does not have, and no silent downgrade (`talosctl rollback` goes back). A machine
    whose schematic Omni cannot tell is not upgraded through the executor. The proxy reads the
    machine's status itself for this, its schematic and version only. The same check runs at
    preflight, so a doomed upgrade is refused before the card; at the run it checks the legacy
    upgrade call, and the image pull and install calls that talosctl makes for Talos 1.13 and
    later.

  Before approval it forwards reads only.

- Refused when requested (`[validation]`): argv is read by a strict subset of both tools' grammar,
  so it reads every word as the binary will: the command's words, then long flags only, as
  `--name` or `--name=value`, and words that do not start with `-`. Short flags, `--`, a flag
  given twice, a value starting with `-`, a flag name with `_` or capitals, and control or
  invisible characters are refused. Then an allowlist holds, each command with its own flags:
  - omnictl: `get`, `cluster status` (reads); `apply --file=-` and `cluster delete`
    (changes with a dry run); `delete <type> <id>`, one resource by name, with no selector.
  - talosctl, which always needs `--nodes=`: `version`, `get`, `service`, `logs`,
    `etcd members|status|alarm list`, `containers`, `stats`, `memory`, `processes`, `mounts`,
    `netstat`, `time`, `list` (reads); `reboot`, `shutdown`, `reset` and `upgrade` (each with
    `--wait=false`: the command returns once the node accepts it; on Talos 1.13 and later an
    upgrade first pulls and installs the image, so give it a `timeout_seconds` to match), `rollback`,
    `service <id> start|stop|restart`, `etcd defrag|forfeit-leadership|alarm disarm|leave|remove-member`.

  No list holds a flag that chooses the endpoint, cluster, configuration or credential
  (`--omniconfig`, `--talosconfig`, `--context`, `--cluster`, `--endpoints`, `--insecure*`), a
  watch or follow, `--dry-run` or `--debug`. `talosctl upgrade` needs `--image=`, an installer image
  with its version tag and no digest (`factory.talos.dev/<platform>-installer/<schematic ID>:v1.x.y` or
  `ghcr.io/siderolabs/installer:v1.x.y`), held to the node's schematic and version by the proxy. Also refused: `get` or `delete` of a type
  that is not on the allowlist, given by its full name, its plural or its singular (`clusters`,
  `cluster`, `Clusters.omni.sidero.dev`; not an abbreviation such as `mc`), or that may hold a
  secret; `get` in the `secrets` namespace; a resource in stdin of a type not on the allowlist or
  with no type; and argv or stdin that looks like a credential (`contains_secret`).

- Refused at preflight: `omnictl cluster status` or `cluster delete` of a cluster not in
  `executor.omni.clusters`, and a resource in stdin that belongs to none of them (label it
  `omni.sidero.dev/cluster`). The proxy checks every write again when it runs.

- Preflight previews `omnictl apply` and `omnictl cluster delete` by running them with
  `--dry-run`, through the proxy forwarding reads only, and the card shows what they printed, 16
  KiB at most. A dry run that fails is refused with its exit code and the gRPC code, or that the
  proxy refused it, not the tool's message; the executor's log keeps it. A read has nothing to
  preview. Every other change cannot be previewed, and the card says so.
- The card shows the exact command, the stdin, the preview, and tags: what it changes (an upgrade
  to a named image says it installs that image as the node's operating system), destructive
  (`delete`, `cluster delete`, `shutdown`, `reset`, `etcd leave`, `etcd remove-member`), the
  number of nodes, no preview, and always Omni Admin. It says how the proxy bounds the command:
  a talosctl command goes only to the target's machines; an omnictl command is held to the
  executor's clusters, though Omni may manage others. A destructive command waits 1 hour for the
  owner.
- The agent's view of the facts has the preview kind, not the dry run's text.

Not allowed in this part: `talosctl patch` and `apply-config` (their preview would read the
machine config, which holds secrets), `dmesg` and `omnictl machine-logs` (the kernel log),
`image pull`, `upgrade-k8s`, and every command that makes
or reads a kubeconfig, talosconfig or service account.

`command.ssh {target, command, stdin, timeout_seconds}` runs one command line on a home-network
host, over ssh, as that host's admin user, with the executor's own key for that host.

- `target` is a host's name from the executor's host allowlist (`executor.ssh.hosts`: a name and an
  IP address with its port, such as `nas`). An address, or a name not on the list, is refused.
- `command` is one line of at most 8 KiB, with no control or invisible characters. The remote
  user's shell receives and interprets it, as ssh always does: it can do anything that user can.
  `stdin` is up to 64 KiB of text that the command reads (a script for `sh -s`, a file's new
  contents); a carriage return is allowed only before a newline, so no line can show as something
  other than what runs. `timeout_seconds` is 1 to 600, default 60. A command or stdin that looks like it holds
  a credential is refused (`contains_secret`): the card shows them as they are.
- The executor's ssh client is its own (Go's `x/crypto/ssh`, no OpenSSH configuration). It accepts
  only the host key pinned for that host, logs in with that host's key only, and asks for the
  command and nothing else: no agent, no port or X11 forwarding, no terminal, no environment.
  Each host's key, user and pinned host key are read for each connection.
- **Preflight.** The executor connects, checks the host key, logs in and disconnects. Nothing runs,
  and nothing can preview the command. The facts are the host, its address, the user and the host
  key's fingerprint; the agent sees them without the address. A host key that is not the pinned
  one, a refused login, or a host the executor cannot reach ends the request as `preflight_failed`,
  with the reason, which names the host but not its address.
- The card shows the command, its stdin, the host, user, address and host key, and tags: remote
  shell, the user and host, and from a scan of the words, which is not a guarantee, destructive
  (`rm`, `dd`, `mkfs`, `Remove-Item`…) and reboot or power off. A command tagged destructive waits
  1 hour for the owner. The card says that the command's output (exit code, stdout and stderr,
  redacted and bounded) goes back to the agent and is stored with the action.
- **Run.** The executor connects again and checks that the host key, user and address are those the
  owner saw. If any changed, it is `stale` and nothing runs. Then it runs the command once, with
  the run's result as for the other commands. Only a failure before the command is sent, or the
  host's explicit refusal of the exec request, is `failed`, "nothing ran". A connection that breaks
  after the command was sent is `uncertain`, since the host may have started it, and it is never
  run again. A command that ends without an exit status (a dropped connection) is `uncertain`. One
  that runs past its timeout is `uncertain` too: the executor asks the host to kill it and closes
  the connection, but with no terminal the host may ignore both, so the command may still be
  running there.

`http.unifi {method, path, body}` sends one request to the UniFi console's Network application,
with the executor's own API key.

- `method` is `GET`, `POST`, `PUT`, `PATCH` or `DELETE`. `path` is at most 2 KiB: the console's
  Network application path (`/proxy/network/integration/...`, `/proxy/network/api/...` or
  `/proxy/network/v2/...`), with an optional query of printable ASCII and no fragment. Another
  application's path, an empty, `.` or `..` segment, and the console's administrators, API keys,
  site management and backups (`admin`, `apikey`, `sitemgr`, `backup` segments) are refused: those
  stay the owner's. `body` is a JSON object or array of at most 64 KiB, for `POST`, `PUT` and
  `PATCH` only. Mainloop stores it with sorted keys and no spaces, and the console receives exactly
  those bytes.
- A body that sets a secret field to a non-empty value is refused (`contains_secret`): a field
  named `x_...`, or one whose name holds password, passphrase, secret, psk, private key, token,
  API key, auth key or credential. The owner sets secrets on the console. A path or body that looks
  like it holds a credential is refused too. The same fields are redacted (`[redacted]`) wherever
  the console's answers show: on the card, in the facts and in the result.
- The executor's HTTPS client is its own. It accepts only the console's pinned certificate (the
  credential's `certificate` field; consoles serve a self-signed one), sends the API key only after
  that check, follows no redirect and opens one connection per request. The key and certificate
  are read for each connection.
- **Preflight.** The executor connects and checks the certificate. For `PUT`, `PATCH` and `DELETE`
  it also sends a `GET` of the same path, and the card shows that answer, redacted, as what is
  there now: at most 16 KiB, so the path must name one object. A path the console has nothing at
  (404), another error, or a larger answer ends the request as `preflight_failed`, as do a
  certificate that is not the pinned one and a console the executor cannot reach; the reason never
  holds its address. A `GET` only reads; nothing can preview a `POST`, and the card says so. The
  agent sees the facts without the console's address or the object.
- The card shows the request, its body, the console's address and certificate fingerprint, and
  tags: UniFi API and the method; "changes the network" for anything but a `GET`; from a scan of
  the words, which is not a guarantee, destructive (a `DELETE`, or delete, forget, reset…) and
  restart or reprovision (restart, upgrade, adopt…); and "no preview" for a `POST`. A request
  tagged destructive waits 1 hour for the owner. The card says the console's answer goes back to
  the agent and is stored with the action.
- **Run.** The executor connects again and checks that the certificate and address are those the
  owner saw, and for a previewed request reads the object again: if any changed, it is `stale` and
  nothing is sent. (A change only to a redacted field is not seen.) Then it sends the request once.
  A failure before any byte was sent is `failed`, "nothing was sent". A connection that breaks
  after that is `uncertain`, since the console may have applied it, and it is never sent again;
  for a `GET` it is `failed`. An answer outside 2xx is `failed`, with the answer; a 2xx is
  `succeeded`, with the status and the body (redacted, bounded) as the result.

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
  that is Mainloop's, the cluster's or Argo CD's; cluster-scoped; access rights; a
  CustomResourceDefinition; creates objects; force_conflicts; for a delete of a Namespace, a
  CustomResourceDefinition or an APIService, what goes with it),
  the labelled agent reason, the expiry, and the action id with the first 16 characters of the
  digest.
- **Options:** `Approve` and `Deny`. The card also accepts a free-text reply.
- **Context:** `privileged_action_id`, `privileged_action_digest`, `privileged_action_type`,
  `repository`, and `pr_url` for a single pull request.

## Decision

The web app sends `Approve` with `metadata.reviewed_context` set to the card's digest. The iPhone
app sends the same with a Touch ID signature, and `serve-api` refuses (`403`) a phone's Approve
without a valid one; see [Signed approvals](#signed-approvals). Either may deny. A sweep records
the decision:

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
uid, creation time, owners, finalizers and Argo tracking. For a patch or an apply they are each
changed object's resolved kind, uid (none for a creation), resourceVersion, Argo tracking and
diff.

An undecided action expires after 24 hours. An approved action that no executor claims within 5
minutes also expires, so a backlog never runs an old approval late.

## Execution

The executor claims an approved action with a 2-minute lease and then:

1. Recomputes the digest from the stored row. If it differs from the approved digest, nothing
   runs and the action fails.
   - It also checks the row is exactly what its type's code reads. The arguments must pass the
     type's validation again. The arguments and the frozen facts must each decode strictly into the
     type's own structs (no unknown field, nothing after the value), and encode back canonically
     to the JSON stored. Go's decoder matches keys in any letter case and keeps the last of two,
     so a row with `"Secret"` beside `"secret"` could show one value and run the other. Such a row
     fails, and nothing runs.
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
- **Failures.** A 403, or another 4xx save a 401, 404, 408, 409 or 429, is `failed`: asking again
  gets the same answer. Other errors are retried like a merge, and each retry observes first.

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

For `argocd.sync` (in `serve-executor`):

- **Observe.** An Application annotated with this action's id, or whose last operation was
  initiated by `mainloop/<action id>`, had its sync started by an earlier attempt, so the action
  `succeeded`. One deleted or replaced, whose resolved revision moved, whose destination, source(s),
  sync options or retry policy changed, or with another set of resources out of sync, is `stale`.
  One that now deploys into the executor (as above) `failed`. A sync pending or running is waited
  for: the attempt is retried.
- **Write.** A merge patch of the Application's `operation`, as the argocd server writes it, pinned
  to the frozen revision(s), with the frozen sync options and retry policy, `initiatedBy`
  `mainloop/<action id>`, the action annotation and the resourceVersion just observed. A concurrent write is retried, observing first.

For `command.kubectl` (in `serve-executor`):

- **Once.** An attempt that finds a `command_started` event ends `uncertain` without running
  anything: an earlier attempt may have run the command and lost its answer.
- **Check.** A change or a delete is previewed again, and it must give exactly what the owner saw
  (resourceVersions aside, which status updates move). Anything else is `stale`, and nothing runs.
  A failed check before the command (a token or network failure) is retried.
- **Run.** The executor extends its lease past the command's deadline, logs `command_started`, and
  runs it. Exit 0 is `succeeded`, another exit is `failed` with the output; a command still running
  at its deadline is stopped and `uncertain`; one that could not be run to its end is `uncertain`.
  The result is the exit code, stdout and stderr.

`command.omnictl` and `command.talosctl` run the same way: once, after the dry run (where there is
one) gives exactly what the owner saw.

For `k8s.patch` and `k8s.apply` (in `serve-executor`), every object is checked before anything is
written:

- **Observe.** A patched object annotated with this action's id was written by an earlier attempt.
  An applied object carries no annotation (it would conflict with the manager that owns the
  annotation), so on a later attempt an object whose approved change is now no change is this
  action's own earlier write. On the first attempt it is `stale`: someone else made the change.
- **Check.** The object is the one the owner saw (the same uid, or still absent for a creation), and
  a fresh dry run gives exactly the approved diff. Anything else is `stale`, naming what an earlier
  attempt already wrote; the result lists it too.
- **Write.** The patch, with the resourceVersion just observed and the action annotation added; or
  the apply, with the resourceVersion just observed. A concurrent write is retried, checking again.

A 403 from Kubernetes, or a write it rejects (another 4xx save a 401, 404, 408, 409 or 429: an
immutable field, an admission webhook's denial), is `failed`, and the result lists what was
written before it. A token, discovery or network failure is retried like a merge.

Each preflight and each attempt has 90 seconds for its remote calls, well inside the 2-minute
lease, so one hung call cannot stall the executor: the attempt is retried, observing first. A
`command.*` attempt has its command's deadline on top.

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

**Kubernetes credentials.** With `executor.kubernetes.enabled`, the chart renders four
ServiceAccounts in the executor's namespace. No pod runs as any of them:

- `mainloop-executor-workloads`: get and patch Deployments, StatefulSets and DaemonSets, and list
  HorizontalPodAutoscalers, cluster-wide
- `mainloop-executor-delete`: get and delete any object, cluster-wide
- `mainloop-executor-writer`: get, create and patch any object, cluster-wide, and `escalate` and
  `bind` on roles, so an approved change can write roles and bindings as an admin's would
- `mainloop-executor-argo`: get and patch Argo CD Applications, cluster-wide, and nothing else of
  Argo CD
- `mainloop-executor-admin`: `cluster-admin`, for `command.kubectl`

The executor's own account may only create tokens for those five. Each run of a `k8s.*` type or
`argocd.sync` mints a 10-minute token for the one it needs; each kubectl run mints one for admin
that outlives its deadline.

A ValidatingAdmissionPolicy, `<namespace>-guard`, denies every write by those five accounts (not
the executor's own) to the executor's namespace and anything in it, to that Namespace, and to the
roles and bindings named `<namespace>-…`. It is a safety net against a mistaken command, not a
boundary against an approved one:

- The API server exempts admission policies and their bindings from every policy made through the
  API, so the guard cannot protect itself. Only the executor's checks and its proxy keep the
  accounts off admission objects.
- It matches the requesting user, so impersonation would change what it sees. The proxy refuses
  impersonation.
- A cluster-admin command can start a workload that writes as another account. The card shows that
  manifest; the guard does not stop it.

**Omni credentials.** With `executor.omni.enabled`, the executor reads an Omni service account's
key (role Admin) from the Secret `executor.omni.keySecret`, key `service-account-key`, and dials
`executor.omni.endpoint`. Only its proxy holds the key; see `command.omnictl` above.

**ssh credentials.** With `executor.ssh.enabled`, each host in `executor.ssh.hosts` has a Secret
`mainloop-executor-ssh-<name>`, with keys `private-key` (an unencrypted OpenSSH key made for the
executor only), `user` and `known-hosts` (the host's key line, pinned; markers and hashed names
are refused). The executor mounts each read-only and reads it for each connection, so a rotated
key needs no restart. The executor reaches a host only if the network lets its address through.

What holds is the executor's: the parser's allowlist, the namespace checks on argv, stdin and the
dry run, the preview's object checks, and the proxy, which holds the token. Reads are not admitted, so the executor refuses commands that name its namespace
or read Secrets before they run. The delete and writer accounts can read every object,
Secrets included, because RBAC cannot grant "everything but Secrets". The executor reads through
delete with metadata-only requests, so no object's data reaches it, and refuses Secrets for the
types that use writer. The guards on Secrets and on the executor's own namespace and roles are the
executor's, not RBAC's.

The split of work:

- **`serve-executor`** preflights an executor type and freezes its facts without opening a card.
  It also claims and runs the approved action.
- **`serve-api`** opens the card, records the decision, expires, delivers the outcome, and raises
  an `uncertain` action to the owner.

An executor type that nobody preflights within 10 minutes ends `preflight_failed`, with a note
that the executor may be down.

`serve-api`'s own database role can still write every column. Without pinned approval keys, a
compromised `serve-api` could therefore forge an approval for an executor type. With them, the
executor runs only what a pinned phone signed; see [Signed approvals](#signed-approvals).

## Signed approvals

A phone approves with its **approval key** ([devices](devices.md#the-approval-key)), whose every
signature asks for Touch ID.

**The token.** The app's Approve carries `metadata.approval`, a compact JWS:

| Part         | Value                                                                       |
| ------------ | --------------------------------------------------------------------------- |
| header `alg` | `ES256`                                                                     |
| header `typ` | `mainloop-approval+jwt`                                                     |
| header `kid` | the device id                                                               |
| `iss`, `sub` | `mainloop-device`, the device id                                            |
| `aud`        | the instance's origin, as for [request tokens](devices.md#signed-requests)  |
| `iat`, `exp` | required; `exp` after `iat` and at most 5 minutes after it (the app uses 2) |
| `jti`        | 16 to 128 characters                                                        |
| `act`        | the action id, as the phone read it from the action's canonical record      |
| `dig`        | the digest, which the phone computed from that record                       |

**What the phone signs.** The digest is the SHA-256 of the action's **canonical record**: the
canonical JSON of the type and its version, the arguments, the frozen facts, the requester, the
action id, the expiry and the catalog version (sorted keys, numbers as written, no HTML escaping).
`GET /privileged-actions/{id}` returns those exact bytes as `canonical_record`. The phone never
takes the card's word for what it approves:

- it hashes the record itself, and refuses unless that is the card's digest and the record names
  the card's action id and type;
- its approval sheet shows only what it parsed from the record: the type, every argument, every
  frozen fact, who asked, the expiry, the action id and the digest, with hidden characters made
  visible ([iPhone app](ios-app.md));
- it signs that digest and that action id.

The executor runs an action only when the record it computes from the stored row has the signed
digest, so a `serve-api` that shows one action and serves another's id or digest gets a token for
what it showed, which the executor refuses for anything else. A test vector written by the Go
server (`server/internal/privileged/testdata/canonical-record.json`) is hashed by the app's tests
to the same digest.

**In `serve-api`**, in the transaction of the answer, an Approve of a privileged card from a phone
is refused (`403`) unless:

- it carries a token, signed by the calling phone's own approval key (a phone registered without
  one is told to sign out and in again);
- the token is for this instance, current (30 seconds of leeway), and names this card's action id
  and digest;
- its `jti` was never used: the token is recorded in `privileged_approvals`, one per action, and a
  `jti` approves one action, ever.

An approval token with an answer from the web app is refused.

**Pinning.** The owner pins phones' approval keys in the chart's `executor.approvalKeys.pinned`:
each entry is the public JWK that Settings › Devices shows, with the device id as `kid`. The chart
gives the same value, `MAINLOOP_PRIVILEGED_APPROVAL_KEYS`, to `serve-api` and `serve-executor`, and
each refuses to start if it is malformed.

`executor.approvalKeys.required` (`MAINLOOP_PRIVILEGED_APPROVAL_KEYS_REQUIRED`, in both processes)
makes this fail closed: the chart refuses to render, and either process refuses to start, with no
key pinned, so a values change that drops the keys can't quietly go back to unsigned approvals.
With any key pinned, or required:

- `serve-api` takes an Approve of an executor type only from a phone whose key is pinned, signed
  with it. The web app's Approve of an executor type is refused ("approve this on your iPhone").
  Types `serve-api` runs itself, such as a merge, can still be approved from the web app.
- `serve-executor` checks, before an action's first step and against its pinned keys only, never
  the database's:
  1. A recorded token exists and a pinned key verifies it for its `kid`.
  2. Its `act` is the action's id and its `dig` the digest just recomputed from the stored row,
     which must also equal the approved digest. A row changed under the same id, however its
     digest was rewritten, no longer matches.
  3. Its `jti` is recorded in the executor's own table, `executor_approval_uses`, for this action.
     A `jti` used for another action is refused.
  4. Every use is bounded by the token's signed `iat`, which `serve-api` can't change, not by the
     executor's record, which it could. A first use more than 15 minutes after `iat` is refused.
     A retry of the same action, which finds its own record, is refused more than 77 minutes after
     `iat`: the 15 minutes, then every attempt at its longest deadline (the remote calls' 90
     seconds and a command's longest timeout, 10 minutes), the 30-second delays between them and
     one 2-minute lease after a crash. A retry later than that needs a new approval.

  A refusal before any attempt ran fails the action with the reason, and nothing runs. A retry
  refused for its age ends **uncertain**, as an earlier attempt may have acted, and is raised to
  the owner.

With no key pinned and not required, an executor type is approved as before, from the web app or a
phone's signed Approve, and both processes log `UNSIGNED PRIVILEGED APPROVALS` as a warning at
every start.

**Limits.**

- `serve-api`'s role owns the schema and could delete the executor's records, turn the row
  triggers off, or reset a finished action to approved. A token still approves only its own
  action and digest, and only until 77 minutes after the owner's Touch ID. Within that time such a
  reset could make the same action run again, which each type's observe-first step mostly absorbs
  (see [Execution](#execution)); after it, the action ends uncertain without running.
- The executor doesn't check the token's `aud`: it has no origin of its own. Action ids are unique
  to an instance and inside the digest, so a token from another instance names no action here.
- Preflight runs before any approval, so a compromised `serve-api` can still request actions and
  read their frozen facts. Signed approvals bound what runs, not what preflight reads.

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
