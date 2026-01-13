#!/usr/bin/env bash
# Reset database and k8s task namespaces
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
CONTEXT="kind-${CLUSTER_NAME}"

echo "=== Using context: ${CONTEXT} ==="

echo "=== Cleaning up k8s task namespaces ==="
# Delete all task-* namespaces (created by worker workflows)
for ns in $(kubectl --context "${CONTEXT}" get ns -o name 2>/dev/null | grep "^namespace/task-" | cut -d/ -f2); do
    echo "Deleting namespace: $ns"
    kubectl --context "${CONTEXT}" delete ns "$ns" --wait=false 2>/dev/null || true
done

echo "=== Resetting database ==="
# Drop both public and dbos schemas to fully reset state
kubectl --context "${CONTEXT}" exec -n mainloop postgres-0 -- psql -U mainloop -d mainloop -c "
DROP SCHEMA IF EXISTS dbos CASCADE;
DROP SCHEMA IF EXISTS public CASCADE;
CREATE SCHEMA public;
"

echo "=== Restarting backend to reinitialize DBOS ==="
# Try DevSpace deployment first, fall back to kind deployment
if kubectl --context "${CONTEXT}" get deployment/mainloop-backend-devspace -n mainloop &>/dev/null; then
    kubectl --context "${CONTEXT}" rollout restart deployment/mainloop-backend-devspace -n mainloop
    kubectl --context "${CONTEXT}" rollout status deployment/mainloop-backend-devspace -n mainloop --timeout=60s
elif kubectl --context "${CONTEXT}" get deployment/mainloop-backend -n mainloop &>/dev/null; then
    kubectl --context "${CONTEXT}" rollout restart deployment/mainloop-backend -n mainloop
    kubectl --context "${CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=60s
fi

echo "=== Reset complete ==="
