#!/usr/bin/env bash
# Reset data between test runs (keep cluster, reset DB + task namespaces)
set -euo pipefail

echo "=== Resetting data ==="

# Delete all task namespaces (created by worker_task_workflow)
echo "Deleting task namespaces..."
kubectl get namespaces -l app.kubernetes.io/managed-by=mainloop -o name 2>/dev/null | \
    xargs -r kubectl delete --wait=false || true

# Reset PostgreSQL database
echo "Resetting PostgreSQL database..."
kubectl exec -n mainloop statefulset/postgres -- \
    psql -U mainloop -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" 2>/dev/null || \
    echo "PostgreSQL not ready or not running"

# Restart backend to clear in-memory state and re-run migrations
echo "Restarting backend..."
kubectl rollout restart deployment/mainloop-backend -n mainloop 2>/dev/null || true
kubectl rollout status deployment/mainloop-backend -n mainloop --timeout=60s 2>/dev/null || true

echo "=== Data reset complete ==="
