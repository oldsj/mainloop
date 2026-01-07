#!/usr/bin/env bash
# Wait for Kind deployments to be ready and healthy
# Ensures test-run doesn't execute against stale code during rollouts
set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
TEST_API_URL="${TEST_API_URL:-http://localhost:8081}"

echo "Waiting for deployments to be ready..."

# Wait for backend deployment (most critical)
if ! kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=120s 2>/dev/null; then
  echo "Error: Backend deployment not ready"
  echo "Check logs: make kind-logs"
  exit 1
fi

# Wait for frontend deployment
if ! kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-frontend -n mainloop --timeout=120s 2>/dev/null; then
  echo "Error: Frontend deployment not ready"
  exit 1
fi

# Verify backend health endpoint responds
echo "Verifying backend health..."
MAX_ATTEMPTS=30
for i in $(seq 1 "${MAX_ATTEMPTS}"); do
  if curl -sf "${TEST_API_URL}/health" >/dev/null 2>&1; then
    echo "✓ Deployments ready, backend healthy"
    exit 0
  fi
  if [[ ${i} -eq ${MAX_ATTEMPTS} ]]; then
    echo "Error: Backend not healthy after ${MAX_ATTEMPTS}s"
    echo "Check logs: make kind-logs"
    exit 1
  fi
  sleep 1
done
