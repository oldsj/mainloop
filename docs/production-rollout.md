# Production deployment

[`charts/mainloop`](../charts/mainloop) deploys a single-owner production instance: the server and web app, an optional CloudNativePG database, and the server's settings as values. It contains no domains, gateway wiring or credential sources; the deployment supplies those (see [Site contract](#site-contract)), so the same chart serves any cluster that provides the prerequisites. [`ci/production-values.yaml`](../charts/mainloop/ci/production-values.yaml) is a complete production values file, and the render tests in `server/internal/deploytest` check it.

Native sessions run as kagent harness Agents on Substrate, so the cluster needs kagent and Substrate before Mainloop can serve a turn. The chart does not install either.

## What the chart renders

| Resource                                                         | Notes                                                                                                                                                                                                     |
| ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Namespace (`createNamespace`), ServiceAccount `mainloop-backend` | The backend's own identity; cross-namespace grants are outside the chart                                                                                                                                  |
| ConfigMap `mainloop-config`                                      | Non-secret settings shared by the server containers (`config`). A checksum of it on the backend pod template rolls the pods when it changes                                                               |
| Deployment `mainloop-backend`                                    | initContainer `migrate` runs `mainloop migrate up`. Three containers from the server image: `backend` (`serve-api`, :8000), `mcp` (`serve-mcp`, :8002) and `git` (`serve-git`, :8003 read and :8004 push) |
| Deployment `mainloop-frontend`                                   | The web image: a static Go server (`web/serve`) on :3000                                                                                                                                                  |
| Services                                                         | `mainloop-backend` (8000), `mainloop-mcp` (80 to 8002), `mainloop-git-read` (80 to 8003), `mainloop-git-push` (80 to 8004), `mainloop-frontend` (3000)                                                    |
| NetworkPolicies                                                  | `mainloop-backend-ingress` (`networkPolicy.backendIngress`) and `mainloop-git-ingress`                                                                                                                    |
| `Cluster/mainloop-pg` (`database.cnpg.enabled`)                  | Single-instance CNPG database. The server connects to `mainloop-pg-rw` directly, with credentials from the generated `mainloop-pg-app` Secret                                                             |

The production values add `strategy: Recreate` (one pod at a time, no surge), read `mainloop-config` and `mainloop-site` only (`importSecretEnv: false`, no whole-Secret import), take the frontend `ORIGIN` from `mainloop-site`, and replace the backend ingress list so only `ate-system` `atenet-egress` reaches `:8002`.

There is no replica hold, archive, backup or staged-promotion machinery.

## Site contract

The chart needs these objects in the release namespace. Any deployment can provide them; the Argo composition for this cluster lives in the infrastructure repository.

| Object                    | Keys                                                                                                  | Used for                                                                                                                                                                                                                                                                                                             |
| ------------------------- | ----------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ConfigMap `mainloop-site` | `FRONTEND_DOMAIN`, `API_DOMAIN`, `FRONTEND_ORIGIN`, `MAINLOOP_OWNER_ID`, `SUBSTRATE_PREVIEW_BASE_URL` | Server CORS, Host guard and owner scoping; the preview host (`<port>--<workspace>--preview.<base host>`); the frontend's `ORIGIN`                                                                                                                                                                                    |
| Secret `mainloop-secrets` | `agent-token-key`, `github-app-id`, `github-app-private-key`                                          | Server GitHub App access (set both App values or neither); key for per-binding agent tokens, read by the API, MCP and Git containers                                                                                                                                                                                 |
| Secret `mainloop-pg-app`  | `username`, `password`                                                                                | Generated by CNPG                                                                                                                                                                                                                                                                                                    |
| Secret `ghcr-secret`      | dockerconfigjson                                                                                      | Image pulls                                                                                                                                                                                                                                                                                                          |
| Gateway routes            | frontend `:3000`, API `:8000`, wildcard preview host `:8000`                                          | Never `:8002`. The preview wildcard must match exactly one label under the base host: with base `https://preview.example.test`, previews are `<port>--<workspace>--preview.preview.example.test`, so route and certify `*.preview.example.test`. Keep it off any wildcard that also covers the frontend or API hosts |
| NetworkPolicy peer        | gateway pods to backend `:8000`                                                                       | Added as its own policy, since the production values replace the default ingress list. The gateway's labels belong to the cluster                                                                                                                                                                                    |
| Images                    | `ghcr.io/oldsj/mainloop-backend`, `ghcr.io/oldsj/mainloop-frontend`                                   | Pinned by digest in each published chart version ([below](#production-changes-are-gitops-only))                                                                                                                                                                                                                      |

The frontend image bakes the API URL at build time (`VITE_API_URL`); the runtime `ORIGIN` does not change it. Build it for the deployment's API domain.

Cross-namespace pieces are owned by the cluster repository, not this chart: the kagent Agents named in `mainloop-config`, the Role granting `mainloop-backend` namespace-wide Secret `get/create/delete` in `kagent`, the MCP `RemoteMCPServer` (source in `k8s/integrations/kagent/`, alongside that Role; the chart does not render it), and policies that let the backend reach kagent `:8083` and the Substrate router `:8081`. Mainloop creates runtime-owned `mainloop-mcp-<binding-id>` Secrets labeled `mainloop.dev/actor-egress=true` and `mainloop.dev/purpose=mcp`. RBAC cannot constrain dynamic creates by name or label; admission policy can restrict writes, while prefix-scoped reads require a separate broker or authorization boundary.

## Production changes are GitOps-only

Argo CD (or any GitOps tool) renders the published chart with the deployment's values. The repository has no Make target that applies, restarts or resets anything in a production cluster. To ship a change:

1. Merge to `main`. The `Publish images` workflow pushes `linux/arm64` images tagged with the first seven characters of the commit SHA.
2. Its `chart` job then packages `charts/mainloop` as `oci://ghcr.io/oldsj/charts/mainloop`, version `0.1.<workflow run number>` and app version `<sha7>`, with both image digests written into the chart's values. It renders the chart and pushes only if both digests appear. Run numbers only grow, so a re-run or a late run of an older commit never outranks a newer chart.
3. A deployment that tracks `0.1.*` (Argo `targetRevision: 0.1.*`) rolls the new chart out. The deployment repository keeps only its values and site objects, and needs no change per release.

`GET /health` reports `revision`, the commit the running image was built from (`dev` for an unstamped build). After a release, compare it with the merged commit.

Mainloop deploys itself this way, so a bad merge can take down the thing that would fix it, and CI cannot smoke production. Every merge also restarts the backend. To roll back, pin the deployment to the previous chart version (each job summary names its version; `helm show chart oci://ghcr.io/oldsj/charts/mainloop --version <v>` shows its app version), then return to `0.1.*` once `main` is fixed.

Changes to the deployment's site ConfigMap do not restart the backend by themselves, because it is read through `envFrom`; change a `server.podAnnotations` value with each site change to roll the pods.

For a local image build, use `docker build --target prod server` for the server and `docker build -f web/Dockerfile .` for the web app.

## Network boundary and preview risk

- Mainloop does no per-request authentication. Reachability (the tailnet and the gateway) is the access control, and `MAINLOOP_OWNER_ID` scopes workspaces and previews. Do not trust `X-User-ID`.
- On `mainloop-backend-ingress`, the production values admit only the actor egress proxy, to `:8002`. The gateway's `:8000` peer comes from the deployment repository as a separate policy, because its labels are cluster-specific. Actors reach MCP only through the credential-injecting egress path. The frontend makes no server-side calls to the backend, so prod grants it nothing.
- Previews run agent-controlled code on a host that shares a registrable domain with the API. This is accepted for a single owner and is not a multi-user boundary. The server's unknown-Host 404 (except `/health`), foreign-Origin write rejection and owner scoping are the controls. Verify them through the real gateway before relying on them, together with wildcard DNS and certificate coverage and WebSocket upgrades.
- Any `NetworkPolicy` evidence needs an allowed control next to each blocked probe, and must consider every policy selecting the destination, including Cilium policies.

## Database

The `migrate` initContainer runs `mainloop migrate up` before each backend pod starts. There is no backup or restore automation here. `Cluster` PVCs follow the storage class's reclaim policy. To start over, delete the `mainloop-pg` Cluster through GitOps (removing it from the render) and let CNPG create a new one; the next `migrate up` recreates the schema. Native session state lives in kagent, not here.

## Smoke after a deploy

Run the read-only smoke in the backend container. It sends no writes and spends no quota. The production image has no Go toolchain; the binary is at `/usr/local/bin/mainloop`:

```sh
kubectl --context <context> -n mainloop exec deploy/mainloop-backend -c backend -- \
  /usr/local/bin/mainloop smoke --read-only --kagent
```

Use an explicit context.

## Verification

```sh
helm template mainloop charts/mainloop -n mainloop -f charts/mainloop/ci/production-values.yaml
```

Check that each server container's `envFrom` lists only `mainloop-config` and `mainloop-site`; `go test ./internal/deploytest/` in `server/` asserts this and more. A repository-only render does not include the site objects above; validate the composed Application on the target cluster.
