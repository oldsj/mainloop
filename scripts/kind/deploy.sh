#!/usr/bin/env bash
# Apply the Kind overlay and wait until PostgreSQL and both app Deployments are ready.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"
CONTEXT="kind-${CLUSTER_NAME}"

if ! kubectl --context "${CONTEXT}" get secret mainloop-secrets -n mainloop >/dev/null 2>&1; then
  echo "ERROR: mainloop-secrets is missing in ${CONTEXT}; run 'make kind-secrets' first" >&2
  exit 1
fi

echo "=== Applying the Kind overlay to ${CONTEXT} ==="
kubectl --context "${CONTEXT}" apply -k "${REPO_ROOT}/k8s/apps/mainloop/overlays/kind"

# The overlay names the images :kind. A rebuild reloads them under the same name, so apply alone
# leaves the old pods running. Restart the app pods to pick up the newly loaded images.
kubectl --context "${CONTEXT}" rollout restart deployment/mainloop-backend deployment/mainloop-frontend -n mainloop

echo "=== Waiting for PostgreSQL and the app ==="
kubectl --context "${CONTEXT}" rollout status statefulset/mainloop-postgres -n mainloop --timeout=180s
kubectl --context "${CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=180s
kubectl --context "${CONTEXT}" rollout status deployment/mainloop-frontend -n mainloop --timeout=180s
kubectl --context "${CONTEXT}" get pods -n mainloop
