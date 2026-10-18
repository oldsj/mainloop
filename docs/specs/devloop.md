# Live dev loop on mainloop-dev

Status: slice 1 (lease, `dev_sync`, `dev_status`) implemented in `server/internal/devloop` and on in
production by owner decision 2026-10-17, not proven live. Slice 2 (`dev_logs`, `dev_smoke`) is not built.

## What it does

A task agent of project `oldsj/mainloop` can iterate on the shared `mainloop-dev` instance
(`docs/dev-environment.md#live-dev-instance`) the way an operator's `make dev-live` does: acquire
the one lease, push its branch through the existing git push transport, sync the pushed commit
into mainloop-dev and read pod status. The task's
workspace never receives a Kubernetes credential or kubeconfig: every devloop MCP tool is served by
`serve-mcp`, which reaches only Mainloop's own database, never Kubernetes. The Kubernetes- and
GitHub-reaching work runs in `serve-api`'s reconcile loop, the same process and credential that
already issue the MCP and git capability Secrets (`server/internal/k8ssecrets`).

**The boundary this does not give you.** The code a task syncs runs inside mainloop-dev's own pods,
with mainloop-dev's credentials, not in the task's workspace. Today those pods share production's
secret set (the agent token key, the GitHub App private key, the kagent control token), their
ServiceAccount can read and write Secrets in the kagent namespace, and the namespace's
NetworkPolicies limit ingress only, so egress is open. A synced commit can therefore reach all of
that. Syncing is a way to run agent-written code with those credentials. The owner accepted this on
2026-10-17: the dev loop exists for faster iteration, not isolation from production, so it is on
in the production overlay (`MAINLOOP_DEVLOOP_ENABLED=true`). The items under "Not built" would
narrow this boundary; none is required to use the feature.

## Tools

Four MCP tools, visible only when `MAINLOOP_DEVLOOP_ENABLED=true` (exactly that string) and only to
a task's workspace credential (role `agent` depth 0, or a supervisor/child holding the workspace
grant — the same population `open_pull_request` is visible to). A further handler-level check
refuses every call whose session is not bound to project `oldsj/mainloop`, because the flag alone
cannot express a project restriction.

- `dev_lease_acquire`, `dev_lease_release`: the one lease on mainloop-dev.
- `dev_sync(commit_sha)`: sync a commit already pushed through the git push transport into
  mainloop-dev. Requires holding the lease.
- `dev_status`: read mainloop-dev's pod status.

`dev_sync` and `dev_status` each write one `devloop_operations` row and poll it for up to a bound
(90s for `dev_sync`, 15s for `dev_status`), returning the row's state and detail either way:
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

One durable row (`devloop_lease`, singleton id `mainloop-dev`): `holder_session_id`,
`acquired_at`, `last_used_at`, `idle_timeout_seconds` (default 1200 = 20 minutes). A FIFO queue
(`devloop_lease_queue`) orders waiting sessions by first request, so a retried or polling acquire
never jumps the line. Every acquire call refreshes the caller's `last_seen_at`; an entry not seen
for two minutes is ignored and removed, so a caller that queued once and left does not hold the
lease for the next caller. When a session ends (any path that revokes its credentials), it loses
the lease if it held it and its queue entry.

`dev_lease_acquire` is DB-only, in one transaction: join the queue (a no-op if already queued);
if the lease is free or idle-expired, grant it to the earliest queued session (not necessarily the
caller); then report whether the caller holds it, or its place in the queue. Calling it again
while already holding it is a heartbeat (`last_used_at` advances). An idle-expired lease is taken
over this way with no explicit release: whichever queued session next calls `dev_lease_acquire`
gets it, in FIFO order. `dev_lease_release` clears the holder and the caller's own queue entry; it
does not itself hand the lease to the next session — that session's own next `dev_lease_acquire`
call does, finding the lease free.

`GET /devloop/lease` (owner API) reads the lease without acquiring it or joining the queue;
`POST /devloop/lease/acquire` and `.../release` act as the fixed owner holder (`devloop.OwnerHolder`,
distinct from any MCP session id), for `make dev-live` (`docs/dev-environment.md`). None of these
three are yet wired into a task-view UI component: a known gap, see Not implemented below.

## Sync

`dev_sync` requires the lease and reads `devloop_operations`'s durable intent for the reconcile
loop to drive (`server/internal/reconcile`):

1. The agent has already pushed its branch through the existing git push transport
   (`docs/specs/push-gate.md`); `dev_sync` takes the resulting commit, not the push itself.
2. `OperationsService`'s loop (run only by `serve-api`) fetches `commit_sha`'s packfile from
   GitHub through the same seeder the git transport already uses to seed a new workspace
   (`pushgate.GitHubUpstream.Seed`, a `want <sha> / done` upload-pack request with no `have`
   lines), unpacks it into a scratch bare repository with a local `git index-pack --stdin`, and
   runs `git archive` for the `server` and `web` trees. No GitHub call other than this read is
   made; nothing is pushed.
3. The archive is filtered and re-rooted per target (`FilterTar`), with the same exclusions
   `devspace.yaml`'s sync already applies (`.git/`, `bin/`, `tmp/`, `testdata/`, `*_test.go`,
   `*.test` for `server/`; `node_modules/`, `dist/`, `e2e/`, `playwright-report/`,
   `test-results/` for `web/`), and copied into each target container by piping a tar stream to
   `tar -x` over a Kubernetes exec (`k8s.io/client-go/tools/remotecommand`): the backend, mcp and
   git containers' `/src`, and the frontend container's `/app/web`. mainloop-dev's existing
   `watchexec` and Vite reload pick up the change; devloop restarts nothing itself.
4. The loop waits a short settle delay (default 3s), because the old backend process keeps
   answering until `watchexec` restarts it. It then runs `curl` against `127.0.0.1:8000/health`
   inside the backend container over exec, once per second, for up to `HealthTimeout` (default
   60s). It does not call mainloop-dev over HTTP: the namespace's NetworkPolicy admits port 8000
   only from the frontend pod and the tailscale gateway, so a call from the production backend
   would be dropped. The result is `succeeded` with `health_ok` true or false. When it is false,
   the detail carries the backend's last 20 log lines, which is where a compile error shows.

Syncs run one at a time. Before copying, the loop takes a database lock row (`devloop_sync_lock`,
held until the sync's deadline plus a margin, so a replica that dies does not block syncs for
good); a sync that finds it taken waits and asks again without counting an attempt. Once it holds
the lock it checks again that its session still holds the lease and that no newer sync from the
session exists. A new `dev_sync` also marks the session's older `pending` and `unknown` syncs
`failed` with detail `superseded by a newer sync from this session`, so an older commit that was
waiting to retry cannot later overwrite a newer one.

A sync fails if it is not done five minutes after it was requested; waiting for the lock and every
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

| Setting                                    | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MAINLOOP_DEVLOOP_ENABLED`                 | Exactly `true` turns on tool visibility ([agentauth.Policy.DevLoop]) and registers the operations reconcile loop in `serve-api`. Off in the base config; on in the prod overlay by owner decision (see the boundary paragraph above).                                                                                                                                                                                                                                                                                                                                                                                                     |
| GitHub App configured                      | Required for `dev_sync`'s fetch step; without it, `dev_sync` fails with a clear detail and the other tools still work.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| In-cluster (`KUBERNETES_SERVICE_HOST` set) | Required for the Kubernetes client (`devloop.NewInClusterPods`); outside a cluster `serve-api` builds no `OperationsService` and every devloop tool answers `[unavailable] devloop is not available`.                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| RBAC                                       | `k8s/apps/mainloop/overlays/prod/devloop/` holds a namespaced `Role` in `mainloop-dev` (`pods` get/list, `pods/log` get, `pods/exec` create) and a `RoleBinding` to the production `mainloop-backend` ServiceAccount. There is no ClusterRole. It is applied by Argo as a second source of `mainloop-dev-config`, pinned to the same mainloop revision as the production app. Namespace mainloop-dev is owned by the cluster repository's Argo Application `mainloop-dev-config` (automated prune and selfHeal). A hand-applied copy is not tracked by it, `make dev-live-purge` deletes it with the namespace, and nothing recreates it. |

No credential, kubeconfig or Kubernetes client ever reaches a workspace: `serve-mcp`'s process
(where every devloop tool handler runs) never imports a Kubernetes client package at all, the same
boundary the package comment on `internal/devloop` and `serveMCP` already state for the credential
Secrets.

## Proven and not proven

Proven by tests (`server/internal/devloop`, `server/internal/mcpserver`), against a fake
Kubernetes pod client, a fake GitHub seeder, and a real local `git`, with scratch PostgreSQL:
lease acquire/queue/idle-expiry/takeover/release; `dev_sync` idempotency (a repeat call while the row is in flight
returns it; a repeat call after it finished syncs again, including after a failure); refusal of
`dev_sync` without the lease, at the tool and again at claim time; the lease heartbeat and expiry
rules; the health wait and the unhealthy-backend detail (against a fake `curl`); the retry cap; recovery from an uncertain copy outcome
(the row moves to `unknown` and a later claim retries the copy to success); every devloop tool
refused with the flag off (not even listed in discovery) and with the flag on but the session
bound to another project, or to none; every devloop write refused after its credential is
revoked (`TestEveryWriteToolIsRefusedAfterItsCredentialIsRevoked`); `FetchArchive` unpacking a real
packfile and archiving real `server/` and `web/` trees; `FilterTar` re-rooting and excluding
entries.

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
call); a cleanup sweep for old `devloop_operations` rows.

Known differences from `devspace dev` sync:

- A file deleted in the commit stays in the pod, because the copy only overwrites.
- A change to `web/package.json` or `pnpm-lock.yaml` does not run `pnpm install`.
- A change to `go.mod` alone does not restart `watchexec`, which watches `.go` and `.sql` files.
- The commit is not checked against what this session pushed; any commit GitHub holds for the
  repository can be synced.

Also proven by tests: syncs copy one at a time (and the test fails without the lock); a newer
sync supersedes an older interrupted one, including the review's overwrite scenario; abandoned
queue entries do not hold the lease; a session that ends drops its hold and its place; a tar exit
status is a definite failure even when its message contains words like "reset"; the five-minute
deadline runs from the request.

Not proven: any run against a real mainloop-dev instance, a deployed image, the RBAC manifests
applied to a live cluster, or `scripts/dev-live.sh`'s lease coordination against a real
control-plane owner API.
