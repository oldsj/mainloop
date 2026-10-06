# Mainloop on the kagent Kind spike

This overlay renders the Mainloop REST and MCP containers, frontend, PostgreSQL StatefulSet,
NetworkPolicies, MCP integration and dedicated main/workspace Agents. It uses an existing kagent
fork installation; it does not install kagent, Substrate, provider credentials or the CNI.
The overlay has not been deployed or rendered as part of the M7 continuation. The supervisor
must bind render, schema validation and live evidence to the candidate before deployment.

Render offline from the repository root:

```sh
kubectl kustomize k8s/apps/mainloop/overlays/kind
```

`backend/tests/runtime/test_k8s_manifests.py` checks the rendered NodePorts, container split,
namespace boundaries, Secret references and workspace retention policy. Rendering does not
validate the fork's CRD schema; validate the Agent, AgentTemplate and Harness resources against
the installed fork before deploying through the authorized spike workflow.

## Installation inputs

- A fresh Mainloop database, with the Kind storage provisioner available for the 1 Gi PVC.
- Existing `mainloop/mainloop-secrets` with keys `db-username`, `db-password`, `github-token`
  (may be empty for public metadata) and a separate nonempty `AGENT_TOKEN_KEY`. Both backend
  processes require the same token key. The overlay contains no credential values.
- Existing `kagent/mainloop-workspace-git`, key `authorization`, containing the installation's
  Git authorization header. Edit `git.origins` in `k8s/integrations/kagent/kind/workspace-agents.yaml`
  to match the permitted repository origins. Per-user Git credentials are a separate follow-up.
- kagent fork CRDs/controller with Harness `git`, `sessionIdleTTL` and `onQuiesce` support;
  Substrate worker pool `kagent-default`, atespace `kagent` and snapshot storage at
  `s3://ate-snapshots/kagent/`. Adjust those installation references when necessary.
- Provider ModelConfigs named `claude-subscription-model` and `codex-subscription-model`.
  Existing child Agents `claude-subscription` and `codex-subscription-https` keep their normal
  TTL/snapshot settings and must bind the `mainloop` RemoteMCPServer. Main and child session
  credentials are injected by the gateway; workspace Agents have no Mainloop MCP tools.
- Replace the example `kagent-claude-harness:kind` and `kagent-codex-harness:kind` workload
  images with the exact reviewed fork builds, accessible to the Substrate worker runtime.
  Loading the Mainloop images into Kind does not make Harness images available to that runtime.
- Load the matching `mainloop-backend:kind` and `mainloop-frontend:kind` images into the target
  Kind cluster using the supervisor's authorized workflow, or edit this overlay's `images`.
  The frontend image must be built with `VITE_API_URL=https://mainloop.example.ts.net:8443`.
  Vite embeds this value at build time; a Deployment environment variable cannot replace it.

The integration bootstraps an empty `kagent/mainloop-agent-tokens` Secret and a Role scoped to
that Secret. GitOps must preserve its runtime-managed data. Mainloop's Role is separate from
the credential provider's permission to read that namespace.

## Exposure settings

The checked-in hostnames are placeholders (`example.ts.net`, `example.test`). Real values belong in
the infrastructure repository or an untracked local overlay (see below), never in this repository.
Change the three settings together. All services below retain their in-cluster Service names.

| Surface  | Service NodePort   | Browser URL / setting                                                                |
| -------- | ------------------ | ------------------------------------------------------------------------------------ |
| Frontend | 30300              | `https://mainloop.example.ts.net`; frontend `ORIGIN` in `deployments.yaml`           |
| REST API | 30800              | `https://mainloop.example.ts.net:8443`; frontend build argument `VITE_API_URL`       |
| Previews | same backend 30800 | TCP listener `:8001`; `SUBSTRATE_PREVIEW_BASE_URL=http://previews.example.test:8001` |

A host-managed Tailscale listener terminates HTTPS for frontend/API and forwards raw TCP for
previews to the Kind node's backend NodePort. Tailscale exposure is an operator action; this
overlay does not create it. Set `FRONTEND_DOMAIN`, `FRONTEND_SCHEME`, `API_DOMAIN` and
`SUBSTRATE_PREVIEW_BASE_URL` in `configmap.yaml`. The Host guard accepts the API name with or
without a port; preview hosts must retain their configured `:8001` port.

### Supplying the real hostnames

Create an untracked overlay that builds on this one, for example
`k8s/apps/mainloop/overlays/kind-local/kustomization.yaml` (add the directory to
`.git/info/exclude`, not to a tracked `.gitignore`):

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../kind
patches:
  - target: { kind: ConfigMap, name: mainloop-config }
    patch: |-
      - {op: replace, path: /data/FRONTEND_DOMAIN, value: mainloop.tailnet.example}
      - {op: replace, path: /data/API_DOMAIN, value: mainloop.tailnet.example}
      - {op: replace, path: /data/SUBSTRATE_PREVIEW_BASE_URL, value: "http://previews.tailnet.example:8001"}
  - target: { kind: Deployment, name: mainloop-frontend }
    patch: |-
      - {op: replace, path: /spec/template/spec/containers/0/env/0/value, value: "https://mainloop.tailnet.example"}
```

Render and apply that directory instead of `overlays/kind`, and build the frontend image with the
matching `VITE_API_URL`. The frontend `ORIGIN` env entry must stay the first `env` item for the
index above; check the render if `deployments.yaml` changes.

## Isolation and retention

The inherited backend ingress policy admits REST from the frontend and the configured Tailscale
Gateway pods, and MCP only from `ate-system` egress pods labelled `app: atenet-egress`. A
host-managed NodePort forward has a different source path: confirm the CNI's node/host traffic
behavior before exposure, and add a source-specific ingress rule if needed. Do not replace the
policy with allow-all. PostgreSQL admits only the Mainloop backend pods.

Stock Kind networking (kindnet) does not enforce NetworkPolicies, but the kagent spike cluster runs
Cilium, which does. Enforcement still needs a fresh blocked-connection proof on the target cluster
before this counts as isolation; on a cluster without a policy-enforcing CNI these policies do
nothing. Gateway/TaskStore/router
policies remain installation-specific and must be validated alongside the kagent stack.

Dedicated `claude-workspace` and `codex-workspace` Agents reference Harnesses with `sessionIdleTTL:
0s`, allowed Git origins and `snapshotPolicy.onQuiesce: Full`. Mainloop suspends idle workspace
compute; kagent must never expire a workspace containing unpushed work. The main Agent also has
TTL zero and is excluded from Mainloop idle suspension. Native compaction remains a harness
configuration and live-evidence follow-up.
