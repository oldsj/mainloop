# Live dev loop on mainloop-dev

Status: implemented in `server/internal/devloop`, disabled by default, not proven live.

## What it does

A task agent of project `oldsj/mainloop` can iterate on the shared `mainloop-dev` instance
(`docs/dev-environment.md#live-dev-instance`) the way an operator's `make dev-live` does: acquire
the one lease, push its branch through the existing git push transport, sync the pushed commit
into mainloop-dev, read pod status and logs, and run the read-only smoke check. The task's
workspace never receives a Kubernetes credential, a kubeconfig, or the dev kagent control token or
GitHub App keys mainloop-dev's own pods hold: every devloop MCP tool is served by `serve-mcp`,
which reaches only Mainloop's own database, never Kubernetes. The Kubernetes- and GitHub-reaching
work runs in `serve-api`'s reconcile loop, the same process and credential that already issue the
MCP and git capability Secrets (`server/internal/k8ssecrets`).

## Tools

Six MCP tools, visible only when `MAINLOOP_DEVLOOP_ENABLED=true` (exactly that string) and only to
a task's workspace credential (role `agent` depth 0, or a supervisor/child holding the workspace
grant — the same population `open_pull_request` is visible to). A further handler-level check
refuses every call whose session is not bound to project `oldsj/mainloop`, because the flag alone
cannot express a project restriction.

- `dev_lease_acquire`, `dev_lease_release`: the one lease on mainloop-dev.
- `dev_sync(commit_sha)`: sync a commit already pushed through the git push transport into
  mainloop-dev. Requires holding the lease.
- `dev_status`, `dev_logs(container, tail_lines)`, `dev_smoke`: read mainloop-dev's pod status,
  tail a container's logs, or run the scripted read-only smoke check.

Each tool but the lease ones writes one `devloop_operations` row (reusing an existing row by an
idempotency key on retry) and polls it for up to a bound (90s for `dev_sync`, 15–30s for the
others), returning the row's state and detail either way: `succeeded`, `failed`, or still
`pending`/`unknown` if the bound passed first. The agent may call the same tool again; `dev_sync`
collapses onto the same row for the same session and commit.

## The lease

One durable row (`devloop_lease`, singleton id `mainloop-dev`): `holder_session_id`,
`acquired_at`, `last_used_at`, `idle_timeout_seconds` (default 1200 = 20 minutes). A FIFO queue
(`devloop_lease_queue`) orders waiting sessions by first request, so a retried or polling acquire
never jumps the line.

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
4. `dev_sync` then polls mainloop-dev's own `/health` for up to `HealthTimeout` (default 60s).

A copy whose outcome could not be observed (the exec stream was cut) moves the row to `unknown`
and the loop retries the copy — not the fetch, and not the push, which already happened outside
this tool. This is the one place this feature departs from the reconcile package's observe-before-
retry rule (`server/internal/reconcile/doc.go`): overwriting the sync directory with the same
commit's tree is idempotent by construction (last write wins, same content), so there is no
distinct remote state to observe first, unlike a git push or a GitHub mutation.

`FetchArchive` sends no `have` lines, so GitHub answers with every object reachable from
`commit_sha`, not a shallow slice. This is a known scaling limit (see Not implemented).

## Operator configuration

| Setting                                    | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MAINLOOP_DEVLOOP_ENABLED`                 | Exactly `true` turns on tool visibility ([agentauth.Policy.DevLoop]) and registers the operations reconcile loop in `serve-api`. Off by default.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| GitHub App configured                      | Required for `dev_sync`'s fetch step; without it, `dev_sync` fails with a clear detail and the other tools still work.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| In-cluster (`KUBERNETES_SERVICE_HOST` set) | Required for the Kubernetes client (`devloop.NewInClusterPods`); outside a cluster `serve-api` builds no `OperationsService` and every devloop tool answers `[unavailable] devloop is not available`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| RBAC                                       | `k8s/apps/mainloop/base/rbac-devloop.yaml` (a `ClusterRole`, cluster-scoped so it is immune to each overlay's namespace rewrite) plus `k8s/apps/mainloop/overlays/prod/devloop/` (a standalone kustomization, like `overlays/dev-live/kagent/`, binding that role to the production `mainloop-backend` ServiceAccount, scoped to namespace `mainloop-dev` alone: `pods` get/list/watch, `pods/log` get, `pods/exec` create). This is a cross-namespace piece the cluster repository's Argo Application must sync separately (`docs/production-rollout.md`'s existing "cross-namespace pieces" paragraph), the same way `k8s/integrations/kagent/` already is; until that Application exists, apply it once by hand: `kubectl --context admin@internal-01 apply -k k8s/apps/mainloop/overlays/prod/devloop`. |

No credential, kubeconfig or Kubernetes client ever reaches a workspace: `serve-mcp`'s process
(where every devloop tool handler runs) never imports a Kubernetes client package at all, the same
boundary the package comment on `internal/devloop` and `serveMCP` already state for the credential
Secrets.

## Proven and not proven

Proven by tests (`server/internal/devloop`, `server/internal/mcpserver`), against a fake
Kubernetes pod client, a fake GitHub seeder, and a real local `git`, with scratch PostgreSQL:
lease acquire/queue/idle-expiry/takeover/release; `dev_sync` idempotency (two calls with the same
session and commit collapse onto one row and one copy); recovery from an uncertain copy outcome
(the row moves to `unknown` and a later claim retries the copy to success); every devloop tool
refused with the flag off (not even listed in discovery) and with the flag on but the session
bound to another project, or to none; every devloop write refused after its credential is
revoked (`TestEveryWriteToolIsRefusedAfterItsCredentialIsRevoked`); `FetchArchive` unpacking a real
packfile and archiving real `server/` and `web/` trees; `FilterTar` re-rooting and excluding
entries.

Not implemented: task-view UI surfacing of the lease holder (the REST endpoint and
`dev_lease_acquire`'s own structured result show it meanwhile); a shallow fetch (every object
reachable from the commit is fetched, not a depth-bounded slice); caching the fetched tree across
a copy retry (a retried sync re-fetches, a safe, read-only GitHub call, rather than re-pushing
anything). Not proven: any run against a real mainloop-dev instance, a deployed image, the RBAC
manifests applied to a live cluster, or `scripts/dev-live.sh`'s lease coordination against a real
control-plane owner API.
