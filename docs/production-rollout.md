# Staged production rollout

**HELD source, not an accepted deployment.** The production overlay is a termination
stage: REST/MCP and frontend replicas are zero, the original `mainloop-db` remains
hibernated, and no archive Job or fresh Cluster is enrolled. Merging still changes
production through Argo; even this stage needs explicit publication approval.
Do not combine stages in one merge or rely on sync waves as human approval gates.

This rollout is separate from the held database-login recovery. It does not repair,
replace or authorize that recovery candidate or its runbook. If recovery has changed
production in the meantime, stop and reconcile a new starting snapshot first.

## Source and publication prerequisites

The overlay was prepared on `1575f0f69f9cccd7c9b5f9baa68ee692e7a807e6`, before the
accepted feature candidate. It **must be rebased onto the supervisor-accepted feature
commit before any publication**, preserving that candidate unchanged. The feature
must include the owner seam, Host and Origin guards, required signing key on both
listeners, schema/reconcile changes and dedicated workspace Agents.

Exact rebase operation, after approval of a commit and identification of the accepted
feature commit: record the overlay commit, run `git rebase --onto <accepted-feature-commit>
1575f0f69f9cccd7c9b5f9baa68ee692e7a807e6 slice-e-prod-overlay`, and inspect the complete
result. The angle-bracket value is an operator input, never a manifest value. Do not
cherry-pick, edit or reset the frozen feature checkout. Resolve overlapping production
recovery edits through supervisor review; never discard them automatically. Rerender
and rebind every check to the resulting source identity.

An accepted feature commit may remain on a held branch. Do not merge that feature
alone to production `main` while waiting for this overlay: publish the reviewed
combined feature-plus-Stage-1 tree atomically so the replica/database hold is present
in the very first production sync. Acceptance of source is distinct from authority
to merge it.

The feature owns removal of `mainloop-backend-secret-reader` ClusterRole and binding
from `base/rbac-backend.yaml`; this slice does not edit base. They still exist on the
staging base. Reject the rebased render if either remains or any cluster-wide Secret
permission survives. Base currently contains no obsolete Substrate settings. The prod
patch replaces both containers' `env` and `envFrom` lists, removes the broad
`mainloop-secrets` import, and keeps only explicit credentials plus `mainloop-config`.
Reject obsolete adapter/broker/shim settings if an integration rebase introduces them.
`SUBSTRATE_ROUTER_ADDRESS` and `SUBSTRATE_PREVIEW_BASE_URL` remain used by the feature's
preview proxy; do not delete them as obsolete.

Before any stage publication, the supervisor must:

- Accept exact feature and infrastructure sources after their outstanding checks and
  independent reviews. Neither is accepted merely because this overlay references it.
- Verify published backend/frontend image digests and exact-commit CI. The frontend
  must embed `VITE_API_URL=https://mainloop-api.olds.network`; runtime `ORIGIN` does not
  set Vite's API URL. Existing `latest` images are inherited only while replicas are
  zero; they must be replaced before activation.
- Verify the real fork chart package and controller/Claude/Codex images, supported
  Talos/certificate APIs, storage, credentials and complete infrastructure rollout.
  Source/templates are not published artifacts or live proof. All five Agents must
  be ready; main/workspace TTL must be zero, workspace Git origins and Full quiesce
  snapshots present, children retain their separate defaults. Main compaction,
  Codex refresh and the shared 8083 single-owner risk remain infrastructure gates.
- Confirm there are no competing writers, HPA, autoscaler, Job or external process
  targeting the old database. Mainloop startup mutates schema. Quieting only one
  container is insufficient: REST and MCP share the backend Deployment.
- Inspect Argo's final composition, including infrastructure Application inline
  patches. Existing inline domain patches agree with this overlay; the extra
  `FRONTEND_DOMAIN` env entry is valid. Do not assume the repo-only render is Argo's
  full policy/manifest union. Keep auto-sync from skipping reviewed stage boundaries.
- Retain old Cluster/PVC/credentials and archive resources in Git. Never use a
  cascading Application/namespace deletion, `Replace=true`, `Force=true`, PVC deletion,
  old-Cluster rename or reinitialization as a reset/rollback technique.

Every stage is a separate reviewed Git diff, render/check record, owner publication
approval, GitOps sync and observed gate. A failed gate stops all later stages. Do not
run direct `kubectl apply`, `scale`, `patch` or `annotate` against Argo-managed resources.

## Configuration contracts

Settings below are traced to feature `backend/src/mainloop/config.py` (case-insensitive
Pydantic settings, with explicit aliases for `MAINLOOP_OWNER_ID` and
`MAINLOOP_API_HOSTS`), `identity.py`, `api.py` and `runtime/preview_proxy.py`.

| Setting | Production contract |
| --- | --- |
| `MAINLOOP_OWNER_ID` | `mainloop-runtime-auth/owner-id`, required on REST and MCP. Actual owner value needs owner confirmation; never infer it from a developer account or kagent service identity. |
| `AGENT_TOKEN_KEY` | `mainloop-runtime-auth/agent-token-key`, independent signing material, required on both containers. Preserve across restarts/releases. |
| `DB_USER`, `DB_PASSWORD` | CNPG-generated `mainloop-feature-db-app/username,password`, not the old DB credentials. |
| `GITHUB_TOKEN` | Existing `mainloop-secrets/github-token`; key existence/permissions still require verification. |
| `KAGENT_USER_ID` | Fixed `mainloop`; changing it is a native Session identity migration. |
| `KAGENT_MAIN_AGENT` | `mainloop-main` |
| `KAGENT_CLAUDE_AGENT`, `KAGENT_CODEX_AGENT` | `claude-subscription`, `codex-subscription-https` (children) |
| `KAGENT_WORKSPACE_CLAUDE_AGENT`, `KAGENT_WORKSPACE_CODEX_AGENT` | `claude-workspace`, `codex-workspace` |
| `KAGENT_GATEWAY_URL`, `KAGENT_NAMESPACE`, `KAGENT_ACTOR_ATESPACE` | kagent controller Service on 8083, `kagent`, `kagent` |
| `SUBSTRATE_ROUTER_ADDRESS` | `http://atenet-router.ate-system.svc.cluster.local:8081` |
| `SUBSTRATE_PREVIEW_BASE_URL` | `https://olds.network`, giving `<port>--<workspace>--preview.olds.network` |
| `FRONTEND_DOMAIN`, `FRONTEND_SCHEME`, `API_DOMAIN`, `MAINLOOP_API_HOSTS` | Explicit frontend/API domains and backend Service DNS names; no wildcard API host admission. |
| `DEV_MODE`, `IS_TEST_ENV` | Both false; no signing-key fallback. |

`staged/50-auth.yaml.template` proposes a separate 1Password item contract. Item
existence, owner identity and signing material are **unverified**. Resolve its path
only from approved inputs. Check controller readiness/key presence without printing
Secret values. Missing required Secrets fail closed; they are not permission to invent
values. Keep `1password.yaml` byte-identical, including Cloudflare resources. Provider
and Git credentials used by native Agents remain infrastructure-owned. Do not enroll
`k8s/integrations/kagent` here: infrastructure owns those objects and runtime token data.

## Network boundary and preview risk

The infrastructure repository defines `tailscale/tailscale-gateway` with its HTTPS
`*.olds.network` listener. Its Envoy dataplane runs in `envoy-gateway-system`, as
recorded in the repository's Tailscale network-policy documentation. The repository
**does not establish actual dataplane pod labels**. Stage 1 therefore removes the base
REST grant entirely. `50-gateway.yaml.template` requires a verified, gateway-specific
pod label key/value together with that namespace, in the **same peer**. If one label
is insufficient to identify only this gateway, add the other verified matchLabels in
the promotion candidate. Never substitute a namespace-only grant or the Gateway's
own metadata labels. A deployment using a different dataplane namespace needs a
reviewed correction, not a broadened policy.

The single existing `mainloop-backend-ingress` policy has its entire ingress list
replaced; adding a restrictive second policy cannot revoke an existing allow. The
repo-only effective backend ingress after resolved promotion must be exactly:

| Source | Destination | Result |
| --- | --- | --- |
| Selected Tailscale gateway Envoy pods in `envoy-gateway-system` | Backend TCP 8000 (REST + previews) | Allow |
| `ate-system` pods labelled `app: atenet-egress` | Backend TCP 8002, via `mainloop-mcp:80` | Allow |
| Egress proxy / actors | Backend TCP 8000 | Deny |
| Gateway | Backend TCP 8002 | Deny |
| Frontend, unrelated pods in either allowed namespace, other namespaces | Backend 8000/8002 | Deny |
| Any peer | Backend 8001 | No listener, Service or HTTPRoute |

Actors reach MCP through the credential-injecting egress path, not directly. This
policy is Mainloop ingress; corresponding actor/egress, gateway 8083 and router
CONNECT restrictions belong to infrastructure. Confirm actual `atenet-egress` labels,
source identity and the **union of every live NetworkPolicy/Cilium policy** selecting
these destinations, including node/host-network exceptions. Use an enforcing CNI and
paired allowed/denied probes against both Service and Pod IPs. A timeout without an
allowed control is not isolation evidence. Do not weaken policies to pass probes.

All HTTPRoutes target 3000 or 8000, never MCP or 8001. Exact frontend/API host routes
coexist with the wildcard preview route. Verify Accepted/ResolvedRefs, certificate,
wildcard DNS, route precedence and WebSocket upgrades through the actual gateway.

Preview code is agent-controlled and same-site with the API under `olds.network`.
The owner chose this single-owner domain arrangement; it is not a multi-user security
boundary. Require the feature's unknown-Host 404 guard (`/health` is its documented
exception), foreign-Origin write rejection, owner scoping, and sensitive-header
stripping. CORS alone is insufficient. Tailnet admission grants the configured owner
identity; `X-User-ID` must not be trusted. Validate browser requests from preview origins
are refused for API mutations and unknown wildcard hosts cannot reach REST. Revisit
site separation and real user authentication before multi-user deployment.

## Stage 1 — terminate, keep the original database asleep

Enroll only the root overlay as supplied, after the source/publication prerequisites.
No files under `staged/` are resources or patches. Record the currently deployed Argo
revision and original Cluster/PVC UIDs first using guarded read-only calls. If the
old Cluster is not the diagnosed hibernated instance, stop and reconcile the baseline.
After the approved sync, run:

```sh
sh k8s/apps/mainloop/overlays/prod/staged/verify-quiet.sh on
```

The gate checks the actual selector, replicas zero, hibernation on, and absence of all
matching Pods, including terminating ones. Every API read checks its exit status;
failures cannot masquerade as an empty Pod list. Waits are finite. Retain the output,
source identity and observed Argo revision. Deployment availability alone is not proof.

## Stage 2 — provision only retained archive storage

On the accepted Stage 1 tree, copy `staged/20-storage.yaml.template` to
`archive-storage.yaml` in this overlay and add only `archive-storage.yaml` to root
`resources`. Preserve the hold, old hibernation and original resources. Review/render,
obtain separate publication approval and sync. Then run the quiet gate with `on` and:

```sh
sh k8s/apps/mainloop/overlays/prod/staged/verify-retention.sh
```

Repository `synology-ssd` declares Retain; actual bound PV evidence is required before
any dump. `Prune=false,Delete=false` protects Argo retention, not StorageClass/PV reclaim
semantics. Check capacity (5Gi must fit the dump), CSI fsGroup support, and archive image
UID/GID 26. If binding requires a consumer, remains Pending, or retention differs,
stop for a reviewed storage-stage change; do not promote the dump spec to force binding.
Confirm original DB PVC retention separately and retain its recorded UID; it is never
deleted by this rollout.

## Stage 3 — wake old DB, archive, independently verify

Only after Stage 2's retained-volume and repeated quiet gates pass, resolve
`VERIFIED_PG_ARCHIVE_IMAGE_AT_DIGEST` in `staged/30-archive.yaml.template` from published
image evidence compatible with the actual source PostgreSQL major version. Copy it to
`archive-jobs.yaml`, add it to root `resources`, and change **only** the hibernation
value in `database-patch.yaml` to `off` (the JSON6902 operation in
`staged/30-wake.yaml.template` specifies that exact change; do not enroll both).
Keep both application replicas zero and all old database/storage resources. Do not
reuse the separate held recovery candidate's patches or Jobs.

After this separate approved sync, use an approved shell with `set -eu`:

```sh
set -eu
sh k8s/apps/mainloop/overlays/prod/staged/verify-quiet.sh off
sh k8s/apps/mainloop/overlays/prod/staged/verify-retention.sh
kubectl --context=admin@internal-01 --request-timeout=30s -n mainloop wait \
  --for=condition=Ready cluster/mainloop-db --timeout=10m
kubectl --context=admin@internal-01 --request-timeout=30s -n mainloop wait \
  --for=condition=complete job/mainloop-feature-archive-dump-20261005 --timeout=25m
kubectl --context=admin@internal-01 --request-timeout=30s -n mainloop wait \
  --for=condition=complete job/mainloop-feature-archive-verify-20261005 --timeout=25m
kubectl --context=admin@internal-01 --request-timeout=10s -n mainloop logs \
  job/mainloop-feature-archive-dump-20261005 --tail=10
kubectl --context=admin@internal-01 --request-timeout=10s -n mainloop logs \
  job/mainloop-feature-archive-verify-20261005 --tail=10
sh k8s/apps/mainloop/overlays/prod/staged/verify-quiet.sh off
sh k8s/apps/mainloop/overlays/prod/staged/verify-retention.sh
```

Record both successful Job UIDs, imageIDs, nonzero bytes and identical SHA-256 checksums,
PV/PVC UIDs, source/render digest and observed Argo revision. A read error, failed Job,
missing log, hash mismatch or stale identity blocks the next stage. Inspect only these
sanitized Job logs, not database contents or Secret values. The dump gets its runtime
CNPG connection credentials through `mainloop-db-app`, with read-only transactions.
Confirm the operator-generated app Secret contract and source cluster identity first.

The verifier mounts the retained PVC read-only, has no DB credentials, checks the
checksum, archive table of contents and full SQL/data stream readability without
printing contents. This is an independent mount/readability proof, **not a restore
proof**. Before committing to fresh production use, require a separately authorized,
isolated restore rehearsal for schema/data/roles/extensions and application recovery;
never restore into the old or fresh production Cluster as a test. Archive contains
sensitive user data; retain restricted storage access and the existing old credentials.

Both Jobs have finite deadlines and no automatic retries; existing partial or final
archive paths refuse replay. On failure preserve evidence and hold. New names/PVCs or
another attempt require separately reviewed authority; never overwrite the archive.

## Stage 4 — create the distinct fresh database

After verified archive evidence, restore rehearsal and separate owner approval, set
old `database-patch.yaml` hibernation back to `on`. Resolve
`VERIFIED_CNPG_VECTOR_IMAGE_AT_DIGEST` in `staged/40-fresh-database.yaml.template` to an
actual published CNPG-compatible image with the vector extension and a supported PG
major; do not assume the operator's default image has vector. Copy to
`fresh-database.yaml` and add it to root `resources`. Preserve archive PVC/Jobs, old
Cluster/Database/Pooler and both replica holds. No old resource is renamed or pruned.

After GitOps sync, require old Cluster Hibernated, unchanged original PVC UID,
`mainloop-feature-db` Ready, the fresh Database's vector extension reconciled, pooler
ready, fresh app Secret key presence and fresh PV retention. Bound checks use explicit
`--context=admin@internal-01` and finite timeouts (10 minutes per readiness wait).
Backend must remain absent (`verify-quiet.sh on`). Inspect status/conditions only; no
Secret values. Fresh init creates `mainloop-feature-db-app`; it does not reuse the old
bootstrap secret. A failure holds activation and preserves both Clusters.

## Stage 5 — resolve access, pin artifacts, activate

Resolve `50-auth`, `50-gateway` and `50-images` templates only after all identity,
Secret, image, gateway and infrastructure gates pass. Copy the resolved auth resource
to `runtime-auth.yaml` and enroll it, replace root `ingress-patch.yaml` with the resolved
gateway patch, and add the resolved image entries to root Kustomization. No token
`${...}` may remain in enrolled resources/patches. Keep both replica holds for this
**separate preparation sync**; require Secret readiness and inspect Argo's final render.

Re-run the whole ingress-union checks and verify the final app DB host and SecretRefs
point only to `mainloop-feature-db`. Confirm the inherited broad Secret RBAC is gone,
owner identity is approved, key is stable, both workload images are pinned, and frontend
API build provenance is correct. Existing broad live policies must be removed by their
own GitOps owner before startup. All required feature/infra checks, fresh independent
Luna pre-review and Astra final review must bind to this final candidate.

Only then, in a **separate owner-approved activation change**, remove the `hold.yaml`
entry from root `patches` (do not remove other patches/resources), verify the rendered
backend/frontend replicas are exactly one, and publish/sync. Require Available within
10 minutes, sanitized startup/DBOS health and correct database binding, followed by
separately authorized bounded single-owner chat, workspace, private clone, preview,
stop/queue, idle/wake, deletion and MCP isolation proofs against exact image identities.
Do not call older live proofs evidence for these images. Argo sync and health alone do
not prove functionality or backup recoverability.

## Stop and rollback

- Before Stage 3: leave old DB asleep and applications held. Preserve any allocated PVC.
- During Stage 3: keep applications held. Preserve failed Jobs/partial archive. After
  gathering evidence, separately approved GitOps can re-hibernate the old Cluster.
- After Stage 4: leave old Cluster/PVC/archive intact and preserve the fresh Cluster,
  credentials and PVC even if startup has not occurred. Never prune it as rollback.
- After activation failure: first restore the zero-replica hold through approved GitOps
  and pass the quiet gate. Keep the new DB and image/Job evidence. Reverting to the old
  application is not automatically safe: schema/startup compatibility must be reviewed.
- Returning to the old DB requires its own reviewed source/image/config candidate and
  publication gate after all writers are quiet. Keep it hibernated until that decision;
  do not point the new feature at it. Restore from the archive, if needed, goes to yet
  another distinct Cluster only after a tested recovery plan. Do not import pre-feature
  kagent bindings or blindly replay uncertain Sessions/prompts.

No cleanup/deletion stage is authorized. Loss of access, uncertain writer state, missing
artifact/credentials, failed verification or exhausted repair allowance stops promotion.

## Verification ownership

Only local syntax/whitespace inspection was authorized for this implementation. The
supervisor must run serial offline Kustomize renders for the root and **each resolved
stage** in separate validation copies, including Argo inline patches. Example root
command (client-side only, not a cluster read):

```sh
kubectl --context=admin@internal-01 kustomize k8s/apps/mainloop/overlays/prod
```

Check duplicate keys, resource identities and local references, no active unresolved
inputs, exact old/fresh separation, held replica counts, retained resources across
stages, no DB wake before termination and Retain evidence, no fresh Cluster before
archive verification, both images pinned before startup, SecretRef/environment names,
all ingress policies' union, route ports and removed broad Secret RBAC after rebase.
Run relevant accepted-feature regressions and exact-commit CI. Live checks above are
future gated work; none are claimed performed by preparing these files.
