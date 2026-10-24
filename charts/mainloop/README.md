# Mainloop chart

Deploys Mainloop's Go server (the REST, MCP and Git listeners in one pod) and the React app, with
an optional database: a CloudNativePG `Cluster` (`database.cnpg.enabled`) or a single local
PostgreSQL StatefulSet (`database.local.enabled`).

CI publishes it on each push to `main` as `oci://ghcr.io/oldsj/charts/mainloop`, version
`0.1.<run number>`, with that commit's image digests pinned in its values. Track `0.1.*` to follow
`main`.

Every setting and its default is in [values.yaml](values.yaml). Example installs, which the render
tests in `server/internal/deploytest` check:

- [values-kind.yaml](values-kind.yaml): a local Kind cluster (`make kind-up`, `make dev`; see
  [docs/dev-environment.md#kind](../../docs/dev-environment.md#kind)).
- [ci/production-values.yaml](ci/production-values.yaml): a production install with CNPG, kagent
  control-plane auth and site settings from a `mainloop-site` ConfigMap.
- [ci/live-dev-values.yaml](ci/live-dev-values.yaml): a live-sync dev instance run by DevSpace
  (`make dev-live`; see [docs/dev-environment.md#live-dev-instance](../../docs/dev-environment.md#live-dev-instance)).

The chart reads, but does not create, Secret `mainloop-secrets` (GitHub App keys, agent token key,
and database credentials when CNPG is off), and with `kagentControlAuth.enabled` Secrets
`kagent-control-token` and `kagent-control-ca`. Routes, gateway network policy peers and kagent
objects belong to the deployment.

## Kind

- Images `mainloop-backend:kind` and `mainloop-frontend:kind`, loaded into Kind; no pull secret.
- Settings for `http://localhost:5173` and `MAINLOOP_ALLOW_RESET=1`, which lets `make dev-reset`
  wipe the database. The frontend `ORIGIN`, `FRONTEND_SCHEME` and `FRONTEND_DOMAIN` must match the
  port forward, so change them together.
- Works without kagent: the web app, the owner API and the MCP and Git listeners (Git transport
  off). Native sessions, workspaces and schedules need kagent, so `GET /schedules` returns 502.
- No GitHub App, and Kind's default CNI does not enforce the network policies. The secrets are
  random values created on your machine; do not reuse them.
