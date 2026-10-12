#!/usr/bin/env bash
# Wipe the local Kind database with `mainloop migrate reset`, then restart the backend so its
# listeners reconnect. This deletes all Mainloop data in that cluster.
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"
CONTEXT="kind-${CLUSTER_NAME}"

echo "=== Resetting the database in ${CONTEXT} ==="
kubectl --context "${CONTEXT}" exec -n mainloop deployment/mainloop-backend -c backend -- /usr/local/bin/mainloop migrate reset

echo "=== Restarting the backend ==="
kubectl --context "${CONTEXT}" rollout restart deployment/mainloop-backend -n mainloop
kubectl --context "${CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=180s
echo "=== Reset complete ==="
echo "If localhost:8000 stops answering, restart 'make dev' to reattach the API port forward."
