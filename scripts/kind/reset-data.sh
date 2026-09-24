#!/usr/bin/env bash
# Reset the local development database
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
CONTEXT="kind-${CLUSTER_NAME}"

echo "=== Using context: ${CONTEXT} ==="

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
