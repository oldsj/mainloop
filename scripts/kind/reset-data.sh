#!/usr/bin/env bash
# Reset data between test runs (keep cluster, reset DB + task namespaces)
set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"

echo "=== Resetting data (context: ${KIND_CONTEXT}) ==="

# Delete all task namespaces (created by worker_task_workflow)
echo "Deleting task namespaces..."
kubectl --context="${KIND_CONTEXT}" get namespaces -l app.kubernetes.io/managed-by=mainloop -o name 2>/dev/null |
  xargs -r kubectl --context="${KIND_CONTEXT}" delete --wait=false || true

# Reset PostgreSQL database
echo "Resetting PostgreSQL database..."
kubectl --context="${KIND_CONTEXT}" exec -n mainloop statefulset/postgres -- \
  psql -U mainloop -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" 2>/dev/null ||
  echo "PostgreSQL not ready or not running"

# Restart backend to clear in-memory state and re-run migrations
echo "Restarting backend..."
kubectl --context="${KIND_CONTEXT}" rollout restart deployment/mainloop-backend -n mainloop 2>/dev/null || true
kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=60s 2>/dev/null || true

echo "=== Data reset complete ==="
