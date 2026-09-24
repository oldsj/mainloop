# Mainloop Substrate preview

> Cutover note (2026-09-24): Herdr was removed; Substrate is now the only workspace runtime.

This local Kind preview runs Mainloop's backend, frontend, and PostgreSQL in `mainloop-control`. The backend uses the headless Claude and Codex actors already provisioned in the preview cluster. This overlay does not create actors or include shim-token values.

## Prerequisites

- The `kind-substrate-preview` cluster, `localhost:5001` registry, Substrate router ingress policy, and headless actors are ready. The cluster lane creates the two referenced `mainloop-shim-*` Secrets in `mainloop-control`, each with a `token` key; the deploy script does not create them.
- Docker, `kubectl`, `kubectl-ate`, `curl`, and `openssl` are installed. Docker can reach the local registry.
- The preview kubeconfig is available at `/tmp/substrate-preview-kubeconfig`.

Set the kubeconfig path once in the shell:

```bash
export SUBSTRATE_PREVIEW_KUBECONFIG=/tmp/substrate-preview-kubeconfig
```

## Build and deploy

```bash
scripts/substrate-preview.sh build
scripts/substrate-preview.sh deploy
scripts/substrate-preview.sh status
```

`build` pushes the backend, frontend, and mirrored PostgreSQL images to `localhost:5001`, checks each pushed manifest by digest, and records those digests in `$XDG_STATE_HOME/mainloop/substrate-preview/image-digests` (or `$HOME/.local/state/mainloop/substrate-preview/image-digests` when `XDG_STATE_HOME` is unset). Run `build` before `deploy`; deploy fails if that state file is missing or invalid. `deploy` renders a temporary copy of the overlay with the recorded digests, then applies it to context `kind-substrate-preview`; the tracked overlay remains tag-based. On first deploy, it creates `mainloop-db-app` with a random password if that Secret is absent. The password is not printed, and subsequent deploys keep the existing Secret.

## Open Mainloop

Run this in a terminal and leave it running; Ctrl+C stops both port-forwards:

```bash
scripts/substrate-preview.sh open
```

Open the printed frontend URL. The backend API docs are at `http://127.0.0.1:8000/docs`.

## Watch actors and logs

The installed `kubectl-ate get actor` command does not provide a watch flag. Poll both atespaces every two seconds to see actors suspend and resume:

```bash
watch -n 2 'kubectl-ate --kubeconfig "$SUBSTRATE_PREVIEW_KUBECONFIG" --context kind-substrate-preview get actor --all-atespaces'
```

In another terminal, follow backend logs (or choose `frontend` or `postgres`):

```bash
scripts/substrate-preview.sh logs
scripts/substrate-preview.sh logs frontend
```

`scripts/substrate-preview.sh status` prints Mainloop pods, Substrate actors, and WorkerPools.

## Tear down

This deletes the `mainloop-control` namespace, its local PostgreSQL data, and shim-token Secrets there. It leaves Substrate actors and cluster-level services alone.

```bash
kubectl --kubeconfig "$SUBSTRATE_PREVIEW_KUBECONFIG" --context kind-substrate-preview \
  delete -k k8s/apps/mainloop/overlays/substrate-preview
```
