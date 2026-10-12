# Kind overlay

Runs the Go server and the React app on a local [Kind](https://kind.sigs.k8s.io) cluster, with a
local PostgreSQL. It is for trying Mainloop out on your own machine. It is not a production or
shared-dev layout.

The recipe is in [docs/dev-environment.md#kind](../../../../../docs/dev-environment.md#kind). This
page lists what the overlay changes and what it leaves out.

## What the overlay adds to base

- Images `mainloop-backend:kind` and `mainloop-frontend:kind` in the default `kind` path. DevSpace
  replaces these with its own generated tags. Nothing is pulled from `ghcr.io`, and no pull secret
  is used.
- `mainloop-postgres`: one PostgreSQL 16 StatefulSet with a 1 Gi volume. The backend reaches it as
  `DB_HOST=mainloop-postgres`.
- Settings for `http://localhost:5173` (the web app) and `MAINLOOP_ALLOW_RESET=1`, which lets
  `make dev-reset` wipe the database. The frontend `ORIGIN`, `FRONTEND_SCHEME` and `FRONTEND_DOMAIN`
  must match the port forward, so change them together.
- Every Service stays ClusterIP. Nothing is exposed on the node; the apps are reached through port
  forwards.

## Works without kagent

- The web app and the owner API (`serve-api`), with `mainloop migrate up` run on each pod start.
- The MCP (`serve-mcp`) and Git (`serve-git`) listeners start. Git transport is off, so `serve-git`
  answers health only.

## Needs kagent (not installed here)

- Native Claude Code and Codex sessions, workspaces and their harness Agents. Schedules too:
  `GET /schedules` returns 502, and the backend logs `kagent gateway unreachable` warnings. Agent
  tool calls to Mainloop need a kagent installation that can reach the MCP Service.

## Left out

- **GitHub.** The App keys in `mainloop-secrets` are empty, so there are no repository pushes, PRs or
  GitHub reads.
- **Network policy.** The policies in base and this overlay are applied, but Kind's default CNI
  (kindnet) does not enforce them.
- **Hardening.** The secrets are random values created on your machine for this cluster only. Keep
  the cluster to yourself, and do not reuse these values anywhere else.

## What has been run

On a local Kind v0.33.0 cluster (kubectl v1.37.0, DevSpace v6.3.21):

- `make kind-up` and `make dev` from an empty namespace: pods ready, `/health` returns 200, the web
  app loads in a browser with no console errors.
- A code change followed by `make dev` rolled the backend and frontend pods to the new images.
- `make dev-reset` wiped and migrated the database; `make dev-stop` removed the namespace.

Not run: sessions, workspaces, schedules, GitHub, and any browser flow beyond the home page and
`/health`.
