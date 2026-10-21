# Live dev loop on a project's staging target

Status: slice 1 (lease, `dev_sync`, `dev_status`) implemented in `server/internal/devloop` and on in
production by owner decision 2026-10-17, not proven live. Slice 2 (`dev_logs`, `dev_smoke`) is not
built. Per-project staging targets (this page's "Staging targets" section; oldsj/mainloop#247 first
slice) are implemented for the `namespace` kind; the `vcluster` kind's abstraction, client
resolution and production wiring (`cmd/mainloop/devloop.go`'s `VClusters` field, second slice) are
all implemented and tested against fakes, but not usable in production until the infra grant
described under "vcluster targets" below exists — proposed until then. Until it does,
`devloop.ErrVClusterNotConfigured` is what every vcluster-kind sync fails with in production, the
same as if `VClusters` were still nil.

## What it does

A task agent of a project with a staging target configured can iterate on it the way an operator's
`make dev-live` does for oldsj/mainloop and mainloop-dev: acquire the target's lease, push its
branch through the existing git push transport, sync the pushed commit into the target and read
pod status. oldsj/mainloop's own target is mainloop-dev
(`docs/dev-environment.md#live-dev-instance`), expressed as that project's configured target,
seeded by migration (`00310_devloop_targets.sql`) onto the lease and sync lock rows the original
devloop migration already created — dev_sync for Mainloop behaves exactly as before slice 1. The
task's workspace never receives a Kubernetes credential or kubeconfig: every devloop MCP tool is
served by `serve-mcp`, which reaches only Mainloop's own database, never Kubernetes. The
Kubernetes- and GitHub-reaching work runs in `serve-api`'s reconcile loop, the same process and
credential that already issue the MCP and git capability Secrets (`server/internal/k8ssecrets`).

**The boundary this does not give you.** The code a task syncs runs inside the target's own pods,
with the target's credentials, not in the task's workspace. For mainloop-dev today those pods share
production's secret set (the agent token key, the GitHub App private key, the kagent control
token), their ServiceAccount can read and write Secrets in the kagent namespace, and the
namespace's NetworkPolicies limit ingress only, so egress is open. A synced commit can therefore
reach all of that. Syncing is a way to run agent-written code with those credentials. The owner
accepted this on 2026-10-17: the dev loop exists for faster iteration, not isolation from
production, so it is on in the production overlay (`MAINLOOP_DEVLOOP_ENABLED=true`). The items
under "Not built" would narrow this boundary; none is required to use the feature.

## Staging targets

Implemented (the `namespace` kind; see "vcluster targets" for `vcluster`). A project's staging
target is Mainloop-side configuration — stored in Mainloop's own database and set through the
owner API, never a file committed to the project's own repository, since the owner works on
arbitrary and third-party repos. One target per project (`devloop_targets`, `project_id` unique).
It is read by `server/internal/devloop`'s `TargetService`, the same package that owns the lease and
sync operations, so #246 (per-project verification settings) can extend the same project-settings
shape rather than inventing a second one.

A target has:

- an id (independent of the project id: it is what the lease and sync lock rows are keyed by,
  outliving any one edit to the target's other fields — see "The lease");
- a kind, `namespace` or `vcluster`, and the Kubernetes location that kind needs: `namespace` alone
  for `namespace`, or `namespace` (the vcluster's _host_ namespace), `vcluster_name` and
  `vcluster_namespace` (the namespace inside the vcluster's own API server) for `vcluster`;
  `vcluster_server` is a further optional field, `vcluster` kind only (rejected on a `namespace`
  target): it overrides the server the vcluster's own kubeconfig Secret names, for a vcluster
  whose `exportKubeConfig.server` only resolves from inside its own pod (e.g.
  `https://localhost:8443`). Normally left unset — the infra side should point
  `exportKubeConfig.server` at the vcluster's in-cluster Service URL instead, which makes a task's
  `dev_sync` work without this override;
- `workload`, a label selector (e.g. `app=mainloop-backend`) identifying the pod that `restart_cmd`,
  `migrate_cmd`, `health_cmd` and `smoke_cmd` run in, and `command_container`, the container inside
  it;
- `sync_paths`, a list of `{selector, container, prefix, dest, exclude}`: which repository subtree
  (`prefix`) a sync copies into which pod and container (`selector`, `container`), where it lands
  (`dest`), and which paths under `prefix` it never writes or deletes (`exclude`) — the same shape
  `FilterTar` and `CopyTree` already took as Go arguments, now read from the row instead of a
  package-level constant;
- four optional commands, `restart_cmd`, `migrate_cmd`, `health_cmd` and `smoke_cmd`, each a plain
  shell command run in `command_container`. A target without one skips that step rather than
  failing it: `restart_cmd` is unused by mainloop-dev (its own `watchexec` already restarts on a
  file change) and by any other target whose command container's own process re-reads the synced
  tree on its own; a target whose workload does not pick up a sync without being restarted (a
  compiled binary started once at container start, say, rather than a file watcher or a `go run`)
  sets `restart_cmd`. A sync path with no `restart_cmd` otherwise behaves exactly like
  mainloop-dev's: the copy lands, and whatever already watches that destination (or re-execs on
  every request) is what picks it up — `dev_sync` itself never restarts a container by name or
  kills a process; `restart_cmd` is the only lever a target has for that. The commit marker that
  guards every command step against a container that silently restarted from its image (see "Sync"
  below) is always checked regardless of whether `health_cmd` is set; without one, health is only
  that check, not a liveness probe.

`GET /projects/{project_id}/devloop-target` and `PUT .../devloop-target` are the owner-only REST
routes (`projects.ProjectsAPI`, like `GET`/`PUT .../merge-policy`): reachable only from the owner's
browser, the same as every other route in the owner API. A task never calls them directly — its
own project's target is read internally by the MCP handlers (`devLoopAuthority`, and
`OperationsService` at claim time), scoped to its own `project_id`, which is what "reads visible to
the project's tasks" means here: a task cannot see another project's target through any path. `PUT`
replaces a target wholesale and validates it (every field required for its kind is set, and
`workload`/`command_container` match one `sync_paths` entry, since that is where the commit marker
lives); replacing an existing target keeps its id, so an edit never orphans a lease already held or
queued for it.

## Tools

Four MCP tools, visible only when `MAINLOOP_DEVLOOP_ENABLED=true` (exactly that string) and only to
a task's workspace credential (role `agent` depth 0, or a supervisor/child holding the workspace
grant — the same population `open_pull_request` is visible to). A further handler-level check
refuses every call whose session's project has no staging target configured, because the flag
alone cannot express that restriction. (Before this project-targets slice, the check instead
refused every project but oldsj/mainloop; a project with a configured target is now exactly the
population allowed, oldsj/mainloop included.)

- `dev_lease_acquire`, `dev_lease_release`: the one lease on the session's project's target.
- `dev_sync(commit_sha)`: sync a commit already pushed through the git push transport into the
  target, run its migrate command (if configured), and check it is healthy (its commit marker, and
  its health command if configured) and passes its smoke command (if configured). Requires holding
  the lease. Its description starts "Use after pushing and before open_pull_request." and says it
  succeeds only when the target is healthy and the smoke passes.
- `dev_status`: read the target's pod status.

`dev_sync` and `dev_status` each write one `devloop_operations` row and poll it for up to a bound
(4 minutes for `dev_sync`, 15s for `dev_status`), returning the row's state and detail either way:
`succeeded`, `failed`, or still `pending`/`unknown` if the bound passed first. The agent may call
the same tool again. The idempotency key is the session and commit. A repeat call while the row is
still `pending`, `running` or `unknown` returns that row and copies nothing extra. A repeat call
after the row finished (`succeeded` or `failed`) resets it and syncs again, so a failed sync is
retried and a second sync of the same commit after the pods were changed by hand really copies.

`dev_sync` is refused with `lease_required` unless the calling session holds the lease. Each
accepted call counts as use of the lease and restarts its idle clock. The check is repeated when
the reconcile loop claims the row, so a sync queued by a session that lost the lease meanwhile
fails with a detail naming the lease and copies nothing.

## The lease

One durable row per target (`devloop_lease`, PK `id` = the target's own id, not a singleton):
`holder_session_id`, `acquired_at`, `last_used_at`, `idle_timeout_seconds` (default 1200 = 20
minutes). A FIFO queue (`devloop_lease_queue`, also keyed by the target id) orders waiting sessions
by first request, so a retried or polling acquire never jumps the line, and two different projects'
targets never contend for the same lease: a session holding one project's lease does not block
another project's `dev_sync`. Every acquire call refreshes the caller's `last_seen_at`; an entry not
seen for two minutes is ignored and removed, so a caller that queued once and left does not hold the
lease for the next caller. When a session ends (any path that revokes its credentials), it loses
the lease if it held it and its queue entry. oldsj/mainloop's target keeps the id `mainloop-dev`
(`devloop.LeaseID`), the same id the original devloop migration seeded this row under, so
`dev_lease_acquire` for Mainloop is unchanged by project targets existing.

`dev_lease_acquire` is DB-only, in one transaction: join the queue (a no-op if already queued);
if the lease is free or idle-expired, grant it to the earliest queued session (not necessarily the
caller); then report whether the caller holds it, or its place in the queue. Calling it again
while already holding it is a heartbeat (`last_used_at` advances). An idle-expired lease is taken
over this way with no explicit release: whichever queued session next calls `dev_lease_acquire`
gets it, in FIFO order. `dev_lease_release` clears the holder and the caller's own queue entry; it
does not itself hand the lease to the next session — that session's own next `dev_lease_acquire`
call does, finding the lease free.

`GET /devloop/lease` (owner API) reads mainloop-dev's own lease specifically (`devloop.LeaseID`)
without acquiring it or joining the queue; `POST /devloop/lease/acquire` and `.../release` act on
it as the fixed owner holder (`devloop.OwnerHolder`, distinct from any MCP session id), for
`make dev-live` (`docs/dev-environment.md`). These three routes are not generalized to an arbitrary
project's target: `make dev-live` and its lease coordination are explicitly out of scope for this
slice (oldsj/mainloop#247's own rename is separate and also out of scope), so they keep naming
mainloop-dev's lease the way they always have. None of these three are yet wired into a task-view
UI component: a known gap, see Not implemented below.

## Sync

`dev_sync` requires the lease and reads `devloop_operations`'s durable intent for the reconcile
loop to drive (`server/internal/reconcile`):

1. The agent has already pushed its branch through the existing git push transport
   (`docs/specs/push-gate.md`); `dev_sync` takes the resulting commit, not the push itself.
2. `OperationsService`'s loop (run only by `serve-api`) fetches `commit_sha`'s packfile from
   GitHub through the same seeder the git transport already uses to seed a new workspace
   (`pushgate.GitHubUpstream.Seed`, a `want <sha> / done` upload-pack request with no `have`
   lines), unpacks it into a scratch bare repository with a local `git index-pack --stdin`, and
   runs `git archive` for every distinct `prefix` the target's `sync_paths` name (`server` and
   `web` for mainloop-dev). No GitHub call other than this read is made; nothing is pushed.
3. The archive is filtered and re-rooted per `sync_paths` entry (`FilterTar`), with that entry's
   own `exclude` list (for mainloop-dev, the same exclusions `devspace.yaml`'s sync already applies:
   `.git/`, `bin/`, `tmp/`, `testdata/`, `*_test.go`, `*.test` for `server/`; `node_modules/`,
   `dist/`, `e2e/`, `playwright-report/`, `test-results/` for `web/`), and mirrored onto its
   `selector`/`container`/`dest` by piping a tar stream to a short `sh` script over a Kubernetes
   exec (`k8s.io/client-go/tools/remotecommand`): for mainloop-dev, the backend, mcp and git
   containers' `/src`, and the frontend container's `/app/web`. mainloop-dev's existing
   `watchexec` and Vite reload pick up the change; devloop restarts nothing itself (its own
   `restart_cmd` is unset, since nothing needs it there — a target without a file watcher would
   configure one).

   **Each copy mirrors the commit.** The archive ends with a manifest of every path it holds. The
   script extracts the archive over the destination, and only if `tar` succeeded and the manifest
   arrived whole, deletes every other path under the destination that the excludes do not cover:
   files and links first, then the directories that are left empty. A file the commit deleted or
   renamed is gone afterwards (so no "redeclared" build failure from the old name, and a deleted
   migration is not embedded and re-applied by a later `migrate up` or `migrate reset`). The
   excluded paths (build caches, `bin/`, `node_modules/`, the test files the filter drops) are
   neither written nor deleted; the exclude rules are the filter's own, so `bin/` means the
   top-level directory only. `watchexec` and Vite see the extraction, then the deletions, and the
   last event is the finished tree. The script needs only POSIX `sh` and `find`, `awk`, `sort`,
   `tail`, `rm`, `rmdir` and `tar`, which GNU (the Go server images) and BusyBox (the alpine web image) both
   provide. A failed extraction deletes nothing; a failed deletion (a read-only directory, say)
   fails the sync with the tool's message and leaves the new files in place. A name with a newline in
   it under the destination makes the script refuse before deleting anything (a commit cannot
   bring one in; `MirrorTar` refuses them). Links are removed as links, never followed.

   A path whose type changes between commits fails the extraction, clearly, and nothing is
   deleted. GNU `tar` (the Go server containers) copes with a file becoming a directory but fails
   on a directory becoming a file ("Cannot open: File exists"); BusyBox `tar` (the frontend)
   fails both ways. A container restart does not clear it, because the image's tree comes from
   `main`: `dev_sync` cannot test such a commit until the image has the new layout, or someone
   removes the old path in the container by hand. The commit marker (for mainloop-dev,
   `/src/.devloop-commit`; in general, `<dest>/.devloop-commit` of the `sync_paths` entry whose
   `selector`/`container` match the target's `workload`/`command_container`) is not excluded: a
   copy removes the previous sync's marker and the sync writes this commit's after the last copy.

4. The sync then earns its verdict by running the target's `restart_cmd` (if set), `migrate_cmd`,
   `health_cmd` and `smoke_cmd` (each skipped, not failed, if the target did not configure it) in
   its command pod over exec, in that order, the first miss ending the sync as `failed` with that
   step's output in the detail. The commit marker is always checked alongside the health command,
   configured or not: this is devloop's own correctness guard, not something a target configures.
   mainloop-dev's own seeded commands are:
   1. `go run ./cmd/mainloop migrate up` from the copied tree (idempotent, holds the migrate lock;
      bounded by `MigrateTimeout`, default 150s). Unmerged migrations land in the shared dev
      database, which is disposable, so a branch's migration can reach it before the branch merges.
      Goose matches applied versions by number only: two branches that add the same version number
      leave the second one's file silently skipped, and a version applied from an abandoned branch
      makes a later, lower main migration fail with "missing (out-of-order)". The detail then names
      the recovery: the owner runs `mainloop migrate reset` in the backend container with
      `MAINLOOP_ALLOW_RESET=1`, which wipes mainloop-dev's data, then syncs again. The backend's
      tree mirrors the synced commit, so the reset applies only that commit's migrations and does
      not bring the abandoned branch's file back. `dev_sync` never resets it. The tail
      of the output (25 lines, 3000 bytes, cut on a character boundary) is attached on failure.
   2. After a short settle delay (default 3s, because the old backend process keeps answering until
      `watchexec` restarts it), one exec per second, for up to `HealthTimeout` (default 180s: a stale
      instance rebuilds all three Go containers under `go run`), that first checks the commit marker
      and then runs `curl` against `127.0.0.1:8000`, `:8002` and `:8003` `/health` (API, MCP, git
      read), so the smoke cannot race the mcp and git rebuilds. It does not call mainloop-dev over
      HTTP: the namespace's NetworkPolicy admits port 8000 only from the frontend pod and the
      tailscale gateway, so a call from the production backend would be dropped. On a miss the detail
      carries the backend's last 20 log lines, which is where a compile error shows.
   3. `go run ./cmd/mainloop smoke --read-only` against the pod's loopback listeners (API `:8000`,
      MCP `:8002`, git read `:8003`, git push `:8004`; mainloop-dev runs with `DEV_MODE`, so the
      Host guard accepts loopback). On mainloop-dev, where git transport and the push gate are off,
      it proves the owner-API reads against the real database, an MCP listener that is up and
      refusing unauthenticated calls, and two git listeners that are up but off (their checks are
      reported as skipped). It sends no chat turn, writes nothing and spends no quota. Bounded by `SmokeTimeout` (default 90s). A
      passing run's summary line is kept; a failing run's output tail is attached.

   Only a sync that passes every configured step is `succeeded`. `health_ok: false` is `failed`,
   never a success with a warning. The row's result records how far the sync got: `migrate_ok`,
   `health_ok` and `smoke` ({`passed`, `summary`}) are each absent until that step ran (or if the
   target did not configure it, for `migrate_ok` and `smoke`).

   **The tree must stay the one the sync wrote.** A target's containers typically keep their synced
   path in a writable layer, so a container restart (a liveness kill after a startup crash, say)
   puts back the image's tree and loses the copy; a replaced pod never had it. Left unchecked, the
   restarted instance would answer healthy and pass the smoke for code that is not the commit under
   test. So the sync records, before its first copy, the name and restart count of every pod its
   `sync_paths` selectors name (for mainloop-dev, the backend and frontend pods), and writes the
   commit to the command pod's marker file after the copies. Every health round, and once more after
   the smoke, it fails the sync if a pod was replaced or deleted or its restart count rose, with the
   last log lines (60, at most 6000 bytes) of the previous instance of each container whose own
   restart count rose (where a crash shows; a container that crashed in an earlier sync is not
   read); the health exec fails it too if the marker does not hold the commit. Restart, migrate,
   health and smoke all run in the command pod recorded before the copy. A backend that crashed on
   an old schema before the migration ran is not restarted by the migration, and the liveness
   restart that follows fails the sync; calling `dev_sync` again copies again.

Pods are picked by label and must be `Running` and not deleting (Ready ones first), so a terminating
or evicted pod that still matches the label is never a target. BusyBox `tar` in the alpine frontend
image warns on stderr while still extracting and exiting 0; those warnings (a skipped PAX header, a
stripped leading `/`) are not failures. Any other `tar` stderr line, and any non-zero exit, is.

`GET /devloop/operations?session_id=` (owner API) lists the session's syncs, newest first (at most 50;
without `session_id`, every session's): id, commit, state, `health_ok`, `migrate_ok`, the smoke
summary, detail and times. This is how the owner reads each verdict without taking the agent's word.

Syncs run one at a time. Before copying, the loop takes a database lock row (`devloop_sync_lock`,
held until the sync's deadline plus a margin, so a replica that dies does not block syncs for
good); a sync that finds it taken waits and asks again without counting an attempt. Once it holds
the lock it checks again that its session still holds the lease and that no newer sync from the
session exists. A new `dev_sync` also marks the session's older `pending` and `unknown` syncs
`failed` with detail `superseded by a newer sync from this session`, so an older commit that was
waiting to retry cannot later overwrite a newer one.

A sync fails if it is not done ten minutes after it was requested; waiting for the lock and every
retry count against that one deadline. A copy that is cut five times fails with "gave up after 5
interrupted copies" instead of staying `unknown` forever.

A copy whose outcome could not be observed (the exec stream was cut, with no exit status from tar)
moves the row to `unknown` and the loop retries the copy. A tar that ran and exited non-zero, or
printed to stderr, is a definite failure and is not retried, whatever its message says — not the fetch, and not the push, which already happened outside
this tool. This is the one place this feature departs from the reconcile package's observe-before-
retry rule (`server/internal/reconcile/doc.go`): overwriting the sync directory with the same
commit's tree is idempotent by construction (last write wins, same content), so there is no
distinct remote state to observe first, unlike a git push or a GitHub mutation.

`FetchArchive` sends no `have` lines, so GitHub answers with every object reachable from
`commit_sha`, not a shallow slice. This is a known scaling limit (see Not implemented).

## Operator configuration

| Setting                                    | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MAINLOOP_DEVLOOP_ENABLED`                 | Exactly `true` turns on tool visibility ([agentauth.Policy.DevLoop]) and registers the operations reconcile loop in `serve-api`. Off in the base config; on in the prod overlay by owner decision (see the boundary paragraph above).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| GitHub App configured                      | Required for `dev_sync`'s fetch step; without it, `dev_sync` fails with a clear detail and the other tools still work.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| In-cluster (`KUBERNETES_SERVICE_HOST` set) | Required for the Kubernetes client (`devloop.NewInClusterPods`); outside a cluster `serve-api` builds no `OperationsService` and every devloop tool answers `[unavailable] devloop is not available`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| RBAC                                       | `k8s/apps/mainloop/overlays/prod/devloop/` holds a namespaced `Role` in `mainloop-dev` (`pods` get/list, `pods/log` get, `pods/exec` create) and a `RoleBinding` to the production `mainloop-backend` ServiceAccount (namespace `mainloop`; confirmed against `k8s/apps/mainloop/base/rbac-backend.yaml`). There is no ClusterRole. It is applied by Argo as a second source of `mainloop-dev-config`, pinned to the same mainloop revision as the production app. Namespace mainloop-dev is owned by the cluster repository's Argo Application `mainloop-dev-config` (automated prune and selfHeal). A hand-applied copy is not tracked by it, `make dev-live-purge` deletes it with the namespace, and nothing recreates it. A `vcluster`-kind target needs a second, separate grant (reading its kubeconfig Secret, plus RBAC inside the vcluster itself); see "vcluster targets" below. Neither exists yet, so a vcluster-kind sync still fails with `devloop.ErrVClusterNotConfigured` in production even though `devloop.OperationsService.VClusters` is now wired (`cmd/mainloop/devloop.go`). |

No credential, kubeconfig or Kubernetes client ever reaches a workspace: `serve-mcp`'s process
(where every devloop tool handler runs) never imports a Kubernetes client package at all, the same
boundary the package comment on `internal/devloop` and `serveMCP` already state for the credential
Secrets.

## vcluster targets

Status: the target-kind abstraction, the `vcluster` kind's client resolution and its production
wiring are all implemented and tested against fakes (`server/internal/devloop/target_test.go`);
proposed until the infra grant below exists, since no vcluster target can reach a real cluster in
production without it.

A `vcluster` target locates its workload inside a [vcluster](https://www.vcluster.com) — a virtual
Kubernetes API server running as a workload of a host cluster — rather than a plain namespace on
the host cluster `serve-api` already reaches. This is the shape kagent's own staging environment
takes: a vcluster named `kv` in host namespace `kagent-staging`, with the workload (controller
Deployment `kagent-controller`) inside the vcluster's own namespace `kagent-dev`.

**How `serve-api` reaches it.** `devloop.VClusterPods`, wired into
`OperationsService.VClusters` in `cmd/mainloop/devloop.go`, reads the vcluster's own kubeconfig
from a Secret in its host namespace — by the vcluster chart's own convention, named
`vc-<vcluster_name>` with the kubeconfig at key `config` — through a `devloop.SecretReader`
(`devloop.K8sSecrets`, built by `devloop.NewInClusterSecrets`, the same in-cluster ServiceAccount
credential `devloop.NewInClusterPods` already uses — no new credential type, and the kubeconfig is
never logged or stored, only read and handed straight to `clientcmd`). It then builds a client-go
client from it (`clientcmd.RESTConfigFromKubeConfig`, optionally overriding the resulting
`rest.Config`'s `Host` with the target's `vcluster_server` field if set, then
`kubernetes.NewForConfig`), scoped to `vcluster_namespace` inside that client. This is exactly the
`namespace`/`vcluster` split `devloop.OperationsService.PodsFor` already dispatches on; the
abstraction does not change between kinds, only how the pod client and namespace are resolved. A
Secret read failure (missing RBAC, or the Secret not existing yet) is reported as
`ErrVClusterNotConfigured` wrapping a detail that names the Secret and namespace it could not
read ("cannot read vc-kv Secret in kagent-staging: ..."), never a panic and never the kubeconfig
itself.

**Why `vcluster_server` exists.** A vcluster's own kubeconfig Secret
(`vcluster.exportKubeConfig`) can name a server that only resolves from inside the vcluster's own
pod, such as `https://localhost:8443`; `serve-api`, reaching it from outside that pod, cannot dial
that address. The infra side should set the vcluster's `exportKubeConfig.server` to its
in-cluster Service URL (reachable from any pod in the host cluster) so this is normally
unnecessary. `vcluster_server` is the escape hatch for a vcluster that cannot be configured that
way: an optional per-target override (`vcluster` kind only; rejected on a `namespace` target) used
in place of whatever server the kubeconfig names.

**What is still missing.** Even with `OperationsService.VClusters` wired, a vcluster-kind sync
cannot reach a real cluster until both of these exist — neither of which this repository's code
can grant, and neither of which this slice changes:

1. A `Role` in namespace `kagent-staging` granting `get` on `secrets` with
   `resourceNames: [vc-kv]` (or whichever vcluster the target names), bound by a `RoleBinding` to
   ServiceAccount `mainloop-backend` in namespace `mainloop` — `serve-api`'s own ServiceAccount,
   confirmed against `k8s/apps/mainloop/base/rbac-backend.yaml` and the existing mainloop-dev
   `RoleBinding` in `k8s/apps/mainloop/overlays/prod/devloop/rolebinding.yaml`, which binds the
   same ServiceAccount. This is additive to that existing mainloop-dev RBAC (see the table above):
   it grants reading one named Secret in a namespace devloop does not otherwise touch, not
   `pods`/`exec` on the host cluster. This grant lives in the infra repository that owns
   `kagent-staging`, not here.
2. Inside the vcluster, the ServiceAccount the read kubeconfig authenticates as needs the same
   `pods` get/list, `pods/log` get, `pods/exec` create RBAC the mainloop-dev `Role` grants today,
   scoped to `kagent-dev`. The vcluster's own RBAC is a second, independent grant: reading the host
   Secret only gets a credential, not Kubernetes permissions inside the vcluster.

Until both exist, `devloop.ErrVClusterNotConfigured` is what every vcluster-kind sync fails with
(wrapping the Secret read's own error when the grant is not there), named clearly in the sync's
detail rather than a timeout, a nil-pointer panic or a hang waiting on a credential that is not
there. Applying either grant is an infra change outside this repository's scope (and outside what
a task workspace's `dev_*` tools can do).

## Proven and not proven

Proven by tests (`server/internal/devloop`, `server/internal/mcpserver`), against a fake
Kubernetes pod client, a fake GitHub seeder, and a real local `git`, with scratch PostgreSQL:
lease acquire/queue/idle-expiry/takeover/release; `dev_sync` idempotency (a repeat call while the row is in flight
returns it; a repeat call after it finished syncs again, including after a failure); refusal of
`dev_sync` without the lease, at the tool and again at claim time; the lease heartbeat and expiry
rules; the health wait, an unhealthy backend failing the sync with its log tail (against a fake `curl`); `migrate up` and the read-only smoke run after the copies and after `/health`, in that order, and a failure of either fails the sync with its output (fake exec; each check mutation-tested); a backend restart or pod replacement after the copy (at the first health probe, mid-wait, or during the smoke) and a missing commit marker each failing the sync (the restart check mutation-tested); a restart while health keeps failing ending the sync at the next round with the restart named, no smoke and no wait for the health deadline (mutation-tested); previous logs read only for containers whose own count rose, 60 lines capped by bytes;
each copy leaving the destination equal to the commit: a deleted file, a renamed file and a deleted migration gone, excluded paths untouched, awkward file names (spaces, a leading dash, glob characters) handled, a repeat copy a no-op, a failed extraction deleting nothing, a failed deletion failing clearly, a newline name refused before any deletion, stale links removed without touching their targets (the link and newline guards mutation-tested), and a plain tar without a manifest refused. The script runs for real in the tests, under this machine's `sh` with GNU tools and, where `busybox` is installed, under BusyBox applets (the deletion mutation-tested); a two-commit sync through the loop checks all four containers; each step's timeout naming its own cap and the sync deadline named as such; goose's out-of-order refusal naming the owner's recovery; a multibyte output tail staying valid UTF-8; pod selection ignoring terminating, evicted and pending pods; harmless BusyBox `tar` warnings accepted and real `tar` errors still failing; the sync history endpoint; the retry cap; recovery from an uncertain copy outcome
(the row moves to `unknown` and a later claim retries the copy to success); every devloop tool
refused with the flag off (not even listed in discovery) and with the flag on but the session
bound to a project with no staging target configured, or to none; every devloop write refused after
its credential is revoked (`TestEveryWriteToolIsRefusedAfterItsCredentialIsRevoked`); `FetchArchive`
unpacking a real packfile and archiving real `server/` and `web/` trees; `FilterTar` re-rooting and
excluding entries.

Also proven (`server/internal/devloop/target_test.go`): a target's lease and sync lock rows are
created alongside it and keep their id across an edit to the target's other fields; two different
projects' targets never share a lease, so holding one does not queue the other; `TargetService.Set`
rejects a `vcluster` target missing its inner location and a `workload`/`command_container` that
matches no `sync_paths` entry; migration `00310_devloop_targets.sql`'s seed (mirrored against the
pre-existing mainloop-dev lease and sync lock rows, since the migrated template predates any
project row to seed against); the `vcluster` kind end to end against a fake Kubernetes pod client
reached through a fake `SecretReader` (copy, migrate, health, smoke all run the same as the
`namespace` kind), a working fake kubeconfig resolving to the vcluster's own inner namespace (not
the host namespace), a failing Secret read wrapping `ErrVClusterNotConfigured` the same as no
factory configured at all with a detail naming the Secret and namespace it could not read, a
`vcluster_server` override taking precedence over the kubeconfig's own server (the built client
dials the override, not the kubeconfig's address), `vcluster_server` round-tripping through
`TargetService.Set`/`Get`, and `TargetService.Set` rejecting `vcluster_server` on a `namespace`
target.

Not built (on in production by owner decision 2026-10-17; these would narrow the boundary above,
and none is required to use it):

- mainloop-dev has its own agent token key, instead of production's.
- mainloop-dev holds no write-capable GitHub App private key.
- mainloop-dev has its own kagent control token, not production's.
- The mainloop-dev ServiceAccount has no broad Secret access in the kagent namespace.
- An egress NetworkPolicy for mainloop-dev, so synced code cannot reach arbitrary destinations.
- The Role and RoleBinding applied by the `mainloop-dev-config` Application (see the RBAC row).
- The production backend pod's mcp and git containers still mount the `mainloop-backend`
  ServiceAccount token, which the RoleBinding would give `pods/exec` in mainloop-dev (and which
  holds the kagent Secret grant). Splitting them from the container that runs the reconcile loop
  is deferred until those grants are reconsidered.

Not implemented: task-view UI surfacing of the lease holder (the REST endpoint and
`dev_lease_acquire`'s own structured result show it meanwhile); `dev_logs` and `dev_smoke` (slice
2); a shallow fetch (every object reachable from the commit is fetched, not a depth-bounded slice);
caching the fetched tree across a copy retry (a retried sync re-fetches, a safe, read-only GitHub
call); a cleanup sweep for old `devloop_operations` rows; the two RBAC grants a `vcluster` target
needs (see "vcluster targets"), so no such target is usable in production yet; an owner-facing UI
for creating or editing a project's staging target (`PUT /projects/{id}/devloop-target` is the only
way today, called directly); a `restart_cmd` worked example (mainloop-dev does not need one).

Known differences from `devspace dev` sync:

- A change to `web/package.json` or `pnpm-lock.yaml` does not run `pnpm install`.
- A change to `go.mod` alone does not restart `watchexec`, which watches `.go` and `.sql` files.
- The commit is not checked against what this session pushed; any commit GitHub holds for the
  repository can be synced.

Also proven by tests: syncs copy one at a time (and the test fails without the lock); a newer
sync supersedes an older interrupted one, including the review's overwrite scenario; abandoned
queue entries do not hold the lease; a session that ends drops its hold and its place; a tar exit
status is a definite failure even when its message contains words like "reset"; the ten-minute
deadline runs from the request.

Not proven: any run against a real mainloop-dev instance (including the in-container `go run` migrate and smoke commands and their working directory and environment), a deployed image, the RBAC manifests
applied to a live cluster, or `scripts/dev-live.sh`'s lease coordination against a real
control-plane owner API.
