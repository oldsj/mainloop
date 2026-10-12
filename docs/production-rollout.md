# Production deployment

`k8s/apps/mainloop/overlays/prod` is a generic, single-owner production overlay: the base plus a CloudNativePG database, with the server's settings made explicit. It contains no domains, gateway wiring, image digests or credential sources. The deployment repository supplies those (see [Site contract](#site-contract)), so the same overlay serves any cluster that provides the prerequisites.

Native sessions run as kagent harness Agents on Substrate, so the cluster needs kagent and Substrate before Mainloop can serve a turn. The overlay does not install either.

## What the base and overlay render

The base (`k8s/apps/mainloop/base`) renders:

| Resource                                                      | Notes                                                                                                                                                                                                     |
| ------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Namespace `mainloop`, ServiceAccount `mainloop-backend`, RBAC | The backend's own permissions; cross-namespace grants are outside the base                                                                                                                                |
| ConfigMap `mainloop-config`                                   | Non-secret settings shared by the server containers                                                                                                                                                       |
| Deployment `mainloop-backend`                                 | initContainer `migrate` runs `mainloop migrate up`. Three containers from the server image: `backend` (`serve-api`, :8000), `mcp` (`serve-mcp`, :8002) and `git` (`serve-git`, :8003 read and :8004 push) |
| Deployment `mainloop-frontend`                                | The web image: a static Go server (`web/serve`) on :3000                                                                                                                                                  |
| Services                                                      | `mainloop-backend` (8000), `mainloop-mcp` (80 to 8002), `mainloop-git-read` (80 to 8003), `mainloop-git-push` (80 to 8004), `mainloop-frontend` (3000)                                                    |
| NetworkPolicies                                               | `mainloop-backend-ingress` and `mainloop-git-ingress`                                                                                                                                                     |

The prod overlay adds:

| Resource                         | Notes                                                                                                                                                                                                                           |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Cluster/mainloop-pg`            | New single-instance CNPG database, 10Gi, on the cluster's default storage class. The server connects to the `mainloop-pg-rw` Service directly                                                                                   |
| `mainloop-config` patch          | DB host and name, `FRONTEND_SCHEME=https`, `DEV_MODE` and `IS_TEST_ENV` false, the `KAGENT_*` Agent names and gateway URL, and `SUBSTRATE_ROUTER_ADDRESS`. The domains come from `mainloop-site`                                |
| Backend patch                    | `strategy: Recreate`, so one pod runs at a time and no surge pod is started. Every server container reads `mainloop-config` and `mainloop-site` only (no whole-Secret import). Database credentials come from `mainloop-pg-app` |
| Frontend patch                   | `ORIGIN` from `mainloop-site`                                                                                                                                                                                                   |
| `mainloop-backend-ingress` patch | Ingress list replaced, not appended: only `ate-system` `atenet-egress` reaches `:8002`. The base's frontend and Tailscale rules on `:8000` are dropped in prod                                                                  |

There is no replica hold, archive, backup or staged-promotion machinery. The database is a new Cluster. A different name from any earlier Cluster means an old data volume is never reattached.

## Site contract

The overlay needs these objects in the `mainloop` namespace. Any deployment can provide them; the Argo composition for this cluster lives in the infrastructure repository.

| Object                    | Keys                                                                                                  | Used for                                                                                                                                                                                                                                                                                                             |
| ------------------------- | ----------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ConfigMap `mainloop-site` | `FRONTEND_DOMAIN`, `API_DOMAIN`, `FRONTEND_ORIGIN`, `MAINLOOP_OWNER_ID`, `SUBSTRATE_PREVIEW_BASE_URL` | Server CORS, Host guard and owner scoping; the preview host (`<port>--<workspace>--preview.<base host>`); the frontend's `ORIGIN`                                                                                                                                                                                    |
| Secret `mainloop-secrets` | `agent-token-key`, `github-app-id`, `github-app-private-key`                                          | Server GitHub App access (set both App values or neither); key for per-binding agent tokens, read by the API, MCP and Git containers                                                                                                                                                                                 |
| Secret `mainloop-pg-app`  | `username`, `password`                                                                                | Generated by CNPG                                                                                                                                                                                                                                                                                                    |
| Secret `ghcr-secret`      | dockerconfigjson                                                                                      | Image pulls                                                                                                                                                                                                                                                                                                          |
| Gateway routes            | frontend `:3000`, API `:8000`, wildcard preview host `:8000`                                          | Never `:8002`. The preview wildcard must match exactly one label under the base host: with base `https://preview.example.test`, previews are `<port>--<workspace>--preview.preview.example.test`, so route and certify `*.preview.example.test`. Keep it off any wildcard that also covers the frontend or API hosts |
| NetworkPolicy peer        | gateway pods to backend `:8000`                                                                       | Added as its own policy, since the prod overlay replaces the base ingress list. The gateway's labels belong to the cluster                                                                                                                                                                                           |
| Images                    | `ghcr.io/oldsj/mainloop-backend`, `ghcr.io/oldsj/mainloop-frontend`                                   | Pin by digest. The `Publish images` workflow publishes them on pushes to `main`                                                                                                                                                                                                                                      |

The frontend image bakes the API URL at build time (`VITE_API_URL`); the runtime `ORIGIN` does not change it. Build it for the deployment's API domain.

Cross-namespace pieces are owned by the cluster repository, not this overlay: the kagent Agents named in `mainloop-config`, the Role granting `mainloop-backend` namespace-wide Secret `get/create/delete` in `kagent`, the MCP `RemoteMCPServer` (`k8s/integrations/kagent` is for Kind only), and policies that let the backend reach kagent `:8083` and the Substrate router `:8081`. Mainloop creates runtime-owned `mainloop-mcp-<binding-id>` Secrets labeled `mainloop.dev/actor-egress=true` and `mainloop.dev/purpose=mcp`. RBAC cannot constrain dynamic creates by name or label; admission policy can restrict writes, while prefix-scoped reads require a separate broker or authorization boundary.

## Production changes are GitOps-only

Argo CD applies this overlay (with the site layer) from Git. The repository has no Make target that applies, restarts or resets anything in a production cluster. To ship a change:

1. Merge to `main`. The `Publish images` workflow pushes `linux/arm64` images tagged with the first seven characters of the commit SHA.
2. Copy the digest references from the job summary into the deployment repository, and pin them there.
3. Let Argo roll them out.

For a local image build, use `docker build --target prod server` for the server and `docker build -f web/Dockerfile .` for the web app.

## Network boundary and preview risk

- Mainloop does no per-request authentication. Reachability (the tailnet and the gateway) is the access control, and `MAINLOOP_OWNER_ID` scopes workspaces and previews. Do not trust `X-User-ID`.
- On `mainloop-backend-ingress`, this overlay admits only the actor egress proxy, to `:8002`. The gateway's `:8000` peer comes from the deployment repository as a separate policy, because its labels are cluster-specific. Actors reach MCP only through the credential-injecting egress path. The frontend makes no server-side calls to the backend, so prod grants it nothing.
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
kubectl kustomize k8s/apps/mainloop/overlays/prod
```

Check that the render has no `$patch` directive and that each server container's `envFrom` lists only `mainloop-config` and `mainloop-site`. A repository-only render does not include the site objects above; validate the composed Application on the target cluster.
