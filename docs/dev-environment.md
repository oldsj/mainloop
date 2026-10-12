# Development environment

Local checks run from the repository root and from `server/` and `web/`. [CONTRIBUTING.md](../CONTRIBUTING.md#verification) lists the commands and [AGENTS.md](../AGENTS.md#verification) lists the matching CI jobs. This page covers the root Make targets, their foreground caps, the local Kind stack, and the shared live dev instance.

## Make targets and caps

The root Make targets run under `scripts/with-timeout.mjs`. A cap is a maximum; a timeout means the check did not finish, not that it failed.

| Target          | Cap   | Runs                                                                             |
| --------------- | ----- | -------------------------------------------------------------------------------- |
| `make lint`     | 120 s | `trunk check --upstream origin/main` (changed files)                             |
| `make fmt`      | 180 s | `trunk fmt --upstream origin/main`, then `trunk check --upstream origin/main -y` |
| `make lint-all` | 240 s | `trunk check -a` (whole repository)                                              |
| `make fmt-all`  | 300 s | `trunk fmt -a`, then `trunk check -a -y`                                         |

`make lint` and `make fmt` compare against `origin/main`, so fetch it first.

Go and web checks have no Make wrapper. Run them directly, as listed in [CONTRIBUTING.md](../CONTRIBUTING.md#verification).

## Kind

Runs the Go server and the React app on a local [Kind](https://kind.sigs.k8s.io) cluster, with PostgreSQL in the same cluster. It needs no shared cluster, 1Password or GitHub App. Secrets are random values generated on your machine and stored only in that cluster.

Prerequisites: Docker, `kind`, `kubectl` and [DevSpace](https://www.devspace.sh). This was tested with kind v0.33.0, kubectl v1.37.0 and DevSpace v6.3.21. The first build pulls base images and needs several GB of free disk. If Docker runs out of space, `docker builder prune -f` clears its build cache.

From the repository root:

```bash
make dev
```

The first run creates the cluster `mainloop-local`, builds both images, deploys them, and forwards the ports. It stays in the foreground and streams logs. When it is ready, it prints:

```text
dev:frontend ports Port forwarding started on: 5173 -> 3000
dev:backend ports Port forwarding started on: 8000 -> 8000
```

Open <http://localhost:5173> for the web app. Check the API with:

```bash
curl http://localhost:8000/health
# {"database":"ok","status":"healthy"}
```

On a first start, the backend pod can show `Init:Error` for about 20 seconds while PostgreSQL comes up. The migrate step retries and then succeeds.

| Command            | What it does                                                                                                                                                                      |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `make dev`         | Creates the cluster if needed, builds and loads the images that changed, deploys, forwards the ports and streams logs. Ctrl-C stops the forwards and logs; the pods keep running. |
| `make dev-reset`   | Deletes all Mainloop data in the cluster and restarts the backend. Then run `make dev` again, because the API forward is tied to the old pod.                                     |
| `make dev-stop`    | Stops DevSpace and deletes the `mainloop` namespace, including the database. The cluster stays.                                                                                   |
| `make kind-delete` | Deletes the whole cluster.                                                                                                                                                        |

After you edit code, run `make dev` again. It rebuilds the images whose inputs changed and rolls their pods. Editing anything under `server/` also rebuilds the web image, because its build context is the whole repository.

Without DevSpace, `make kind-up` creates the cluster, builds and loads the images, creates the secrets, and deploys. Then `make kind-forward` forwards the same two ports. Stop `make kind-forward` before `make dev`, because both use those ports.

`KIND_CLUSTER_NAME=<name>` picks another cluster, with kubectl context `kind-<name>`. The commands pass their context explicitly. Creating a cluster does switch your kubectl current context to it.

What works without kagent: the web app, the REST API (migrations run on start), and the MCP and Git listeners, which start with Git transport off. Not available: native Claude Code and Codex sessions, workspaces, and schedules. They need a kagent installation that this stack does not set up, so `/schedules` returns 502. GitHub is not configured, so there are no repository reads or pushes. The [Kind overlay README](../k8s/apps/mainloop/overlays/kind/README.md) lists what the overlay changes.

## Live dev instance

`mainloop-dev` is a shared, prototype instance for fast iteration. It runs in namespace `mainloop-dev` on the production cluster, next to production `mainloop`, but DevSpace deploys and owns it directly; it is not Argo-managed. Production still changes only through pull requests, CI and GitOps.

```bash
make dev-live          # deploy mainloop-dev, then sync and stream logs (Ctrl-C stops sync)
make dev-live-status   # pods, database, route and dev agents
make dev-agents        # apply the *-dev kagent agents (shared kagent namespace)
make dev-harness       # build arm64 harness images from ../kagent, apply them to the dev agents
make dev-live-purge    # delete namespace mainloop-dev and the *-dev agents (typed confirmation)
```

All targets use kube context `admin@internal-01` through DevSpace profile `live` in `devspace.yaml`, whose pipelines (`build`, `dev`, `deploy`, `deploy-agents`, `purge`) refuse any other context or namespace. Use these targets or `scripts/dev-live.sh`: raw `devspace sync`, `restart`, `enter`, `logs` and `attach` with `-p live` are not guarded and act on whatever `--kube-context` and `-n` say. This page covers only the `live` profile.

What runs and syncs: the Go server and the React app.

- `server/` syncs into `/src` in the backend, MCP and Git containers. Each runs `watchexec` over `go run ./cmd/mainloop serve-api|serve-mcp|serve-git`, so a change rebuilds and restarts only that listener. A change to `go.mod` or `go.sum` runs `go mod download` in the pod. Test files, `testdata`, `.git` and build output do not sync.
- `web/` syncs into `/app/web` in the frontend, where Vite hot-reloads. A change to `web/package.json` or `pnpm-lock.yaml` runs the workspace `pnpm install` in the pod. `node_modules`, `dist` and test output do not sync.
- The images already hold the source they were built from, so a pod serves before the first sync lands. A `migrate` initContainer runs `mainloop migrate up` on every pod start, but it sees only that baked-in source, never synced files. After syncing a new migration, apply it yourself in the backend container:

  ```bash
  kubectl --context admin@internal-01 -n mainloop-dev exec deploy/mainloop-backend -c backend -- \
    go run ./cmd/mainloop migrate up
  ```

  (Under `devspace dev` the serving pod is `mainloop-backend-devspace`; see Reset below for addressing it.)

The UI is `https://mainloop-dev.olds.network` (tailnet only, through the same Envoy gateway as production). The browser calls `/api` on that host, and Vite forwards it to the backend Service (`MAINLOOP_API_PROXY`), so the backend needs no route of its own. Vite accepts the gateway host through `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS`.

Images: the **Publish live dev images** workflow builds arm64 images on each push to `main` and pushes `ghcr.io/oldsj/mainloop-server-dev` (`server/Dockerfile --target dev`: Go toolchain, `git`, `watchexec`) and `ghcr.io/oldsj/mainloop-web-dev` (`web/Dockerfile --target dev`: node 24, pnpm, Vite on `0.0.0.0:3000`), each tagged `<short SHA>` and `main`. The overlay pulls the `main` tag. The pods pull the moving tag and the sync overlays the working tree. The pods carry no pull secret, so after the first build of each of the two `-dev` packages make it public once in the GitHub UI (package settings, change visibility); until then the pods sit in `ImagePullBackOff`.

Data: a one-instance CloudNativePG cluster on `local-path` storage. CNPG creates the database credentials. Purging deletes the data.

Reset: the overlay sets `MAINLOOP_ALLOW_RESET=1`, which lets `mainloop migrate reset` drop the schema and migrate back to the latest version. Run it in the backend container of the pod that is serving, then delete that pod so the listeners reconnect. While `devspace dev` is running (`make dev-live`), the serving pod is `mainloop-backend-devspace` and the Deployment's own pod is replaced; otherwise it is the Deployment's `mainloop-backend` pod. Select it by label so both states work:

```bash
K="kubectl --context admin@internal-01 -n mainloop-dev"
POD=$($K get pod -l app=mainloop-backend -o name | head -n1)
$K exec "$POD" -c backend -- go run ./cmd/mainloop migrate reset
$K delete "$POD"
```

The Go server keeps its tables in the `mainloop` schema. A database first used by the retired Python backend may still have tables in `public`. `migrate reset` does not touch them. To drop them as well:
`kubectl --context admin@internal-01 -n mainloop-dev exec mainloop-pg-1 -c postgres -- psql -d mainloop -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'`.
`make dev-live-purge` drops all the data, along with the namespace.

Settings (`k8s/apps/mainloop/overlays/dev-live`): `DEV_MODE=true`; merge tools, owner writes, Git transport and the push gate are off; every kagent role uses `claude-workspace-dev` or `codex-workspace-dev`; `MAINLOOP_MCP_HOSTS` lists the `mainloop-mcp` Service names in `mainloop-dev`; the `control-auth` component is on. Dev work targets `oldsj/testrepo` only through the dev agents' instructions: Mainloop has no repository allowlist, and the shared production GitHub App can reach every repository it is installed on. With Git transport off, nothing reaches GitHub today.

Secrets are not in this repository. The infrastructure repository supplies them from production's existing items (mainloop-dev shares production's GitHub App, agent token key and kagent control principal). The server needs `agent-token-key` and the kagent control Secrets to start, because it has no fallback for the token key; the GitHub App keys are optional, so without them the instance runs with GitHub unavailable:

| Secret in `mainloop-dev` | Keys                                      | Without it                                              |
| ------------------------ | ----------------------------------------- | ------------------------------------------------------- |
| `mainloop-secrets`       | `agent-token-key`                         | The backend, MCP and Git containers do not start        |
| `mainloop-secrets`       | `github-app-id`, `github-app-private-key` | No GitHub access (set both or neither)                  |
| `kagent-control-token`   | `current`                                 | The pods wait on the missing volume; no kagent sessions |
| `kagent-control-ca`      | `ca.crt` (cert-manager)                   | As above                                                |

Agent paths are per namespace. `MAINLOOP_NAMESPACE` (default `mainloop`, set to `mainloop-dev` here) sets the origins agents call, `http://mainloop-mcp.<ns>.svc.cluster.local` and `http://mainloop-git-{read,push}.<ns>.svc.cluster.local`, the MCP server's accepted Host, and the prefix of the per-binding credential Secrets Mainloop writes in the shared `kagent` namespace. The prefix is the namespace itself, so production keeps `mainloop-mcp-*` and `mainloop-git-{read,push}-*`, and mainloop-dev writes `mainloop-dev-mcp-*` and `mainloop-dev-git-{read,push}-*`. Secrets are deleted only by exact name. The Git origin settings accept only the configured namespace's two origins.

`make dev-agents` also applies a RoleBinding (`mainloop-dev-agent-tokens`, in `kagent`) that gives `mainloop-dev:mainloop-backend` production's `mainloop-agent-tokens` Role, so it can create, read and delete its credential Secrets there. Only the API container writes them; the MCP and Git containers verify tokens against the database and need nothing more. kagent's service policy (infrastructure repository) accepts mainloop-dev's MCP credential tuple (`mainloop-dev-mcp-*`, origin `http://mainloop-mcp.mainloop-dev.svc.cluster.local`), so the dev agents can call Mainloop tools.

Git through the dev proxies also needs a kagent change. kagent's service policy (`controlauth/policy.go` in the `oldsj/kagent` fork) accepts only production's Git origins, `http://mainloop-git-{read,push}.mainloop.svc.cluster.local`, with the `mainloop-git-{read,push}-*` patterns. A Git tuple for mainloop-dev would make the policy invalid and stop the shared kagent controller, so mainloop-dev keeps Git transport off.

Production and mainloop-dev share one kagent principal, so each can list the other's Sessions. `mainloop sweep-orphans` and unbound HITL discovery only consider Sessions on the Agents their own settings name (`KAGENT_*_AGENT` and provider profiles), so keep the two instances' Agents disjoint, as the `*-dev` names do. HITL for an existing local binding retains its pinned Agent identity after configuration changes; creator, ownership, lifecycle and stored identity checks still apply.

Smoke: `mainloop smoke --read-only` probes a running instance: health, the owner reads, the event stream, the MCP and Git listeners' refusals, and with `--kagent` the kagent probe. It sends no writes and spends no quota, so run it after each deploy. Run it in the backend container, where the in-cluster Service names resolve:

```bash
kubectl --context admin@internal-01 -n mainloop-dev exec deploy/mainloop-backend -c backend -- \
  go run ./cmd/mainloop smoke --read-only --kagent
```

Git transport is off here, so `git-read` and `git-push` report `skip` and no run can reach merge on this instance. The live run (`smoke --live`) therefore needs an instance with git transport on. It drives one task from chat to merge and spends quota. Run it from an operator machine, not the pod: the pod has no `gh` login, and the live path reads GitHub through the operator's `gh auth status`. It checks `gh` before anything else and refuses, spending no budget, when `gh` is missing or not logged in. Open two port-forwards first, each in its own terminal:

```bash
kubectl --context admin@internal-01 -n mainloop-dev port-forward svc/mainloop-backend 8000:8000
kubectl --context admin@internal-01 -n mainloop-dev port-forward svc/mainloop-git-push 8004:80
```

Then, from `server/`:

```bash
go run ./cmd/mainloop smoke --live --budget 1 \
  --base-url http://127.0.0.1:8000 --git-push-url http://127.0.0.1:8004 \
  --project-id <project-id> --repo <owner/name> --app-login <app-login> \
  --provider claude --merge-tools <on|off>
```

Every target is named on the command line; nothing defaults. `--budget` is a ceiling from 1 to 3,
but every run's outcome ends it, so one run starts. Set `--merge-tools` to match the instance:
with `off`, a run stops after CI and proves no merge. Pass `--api-host` and `--git-push-host` only when the target is not
in dev mode: outside it each listener refuses a Host it does not serve, and a port-forward sends
`127.0.0.1`. See [live-smoke.md](live-smoke.md). Only a run that reaches completion proves the
full path.

Teardown: `make dev-live-purge` deletes everything DevSpace deployed, including the namespace, the database and the dev agents.

Previews do not work on this instance. The overlay sets no `SUBSTRATE_PREVIEW_BASE_URL`, so
the ports API returns `http://<port>--<workspace>--preview.localhost:8001` links. No gateway
route serves a preview wildcard for the dev host. The `atenet-router-ingress` NetworkPolicy in
`ate-system` admits port 8081 only from the `mainloop` namespace's backend, so the proxy's
tunnel dial from `mainloop-dev` times out and the proxy answers 503 "Workspace waking up" on
every request. Proving previews needs an infra change (a base domain, a wildcard route and a
policy entry for `mainloop-dev`); until then they are proven in production.

### Testing approvals on dev

The overlay's `claude-workspace-dev-template` has no approval gate. To test the Approve and
Reject flow on this instance, gate a tool temporarily and restore the template afterwards.

1. Add `tools: [whoami]` and `requireApproval: true` to the template's `mcp` binding (next to
   `server`), and apply it with `kubectl --context admin@internal-01 -n kagent apply`. Do not
   commit the change; the overlay stays ungated.
2. A template change reaches only new native sessions. Switch the main chat's provider away and
   back (`PUT /api/main-thread/provider` with `codex`, then `claude`) so the main chat starts a
   fresh session on the new template.
3. Ask the main chat to call `whoami`. The turn parks and raises a request in Inbox and in the
   chat. Answer it within 5 minutes. A turn parked for about 6 minutes loses its tool call:
   the agent-side connection is cut at about 360 s, an approval after that resumes into a
   dead call, and the agent reports "The call timed out." This is a known kagent issue; Mainloop's
   request stays pending and answerable meanwhile.
4. Restore: re-apply the template from `k8s/apps/mainloop/overlays/dev-live/kagent/claude.yaml`
   (`make dev-agents`), then switch the provider away and back again so the main chat drops the
   gated session.
