#!/usr/bin/env bash
# Deploy mainloop to Kind cluster
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"

echo "=== Deploying to Kind cluster ==="
echo "Using context: ${KIND_CONTEXT}"

# Delete old deployments to avoid waiting for termination
echo "Cleaning up old deployments..."
kubectl --context="${KIND_CONTEXT}" delete deployment --all -n mainloop --ignore-not-found=true --wait=false
kubectl --context="${KIND_CONTEXT}" delete pod --all -n mainloop --ignore-not-found=true --wait=false --force --grace-period=0

# Apply test overlay
echo "Applying manifests..."
kubectl --context="${KIND_CONTEXT}" apply -k "${REPO_ROOT}/k8s/apps/mainloop/overlays/test" --server-side

# Wait for deployments
echo "Waiting for deployments..."
kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-backend -n mainloop --timeout=120s
kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-frontend -n mainloop --timeout=120s
kubectl --context="${KIND_CONTEXT}" rollout status deployment/mainloop-agent-controller -n mainloop --timeout=120s
kubectl --context="${KIND_CONTEXT}" rollout status statefulset/postgres -n mainloop --timeout=120s

echo "=== Deployment complete ==="
echo "Frontend: http://localhost:3000"
echo "Backend:  http://localhost:8000"
kubectl get pods -n mainloop
