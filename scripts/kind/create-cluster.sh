#!/usr/bin/env bash
# Create Kind cluster for mainloop testing
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"

echo "=== Creating Kind cluster: ${CLUSTER_NAME} ==="

# Check if cluster already exists
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Cluster ${CLUSTER_NAME} already exists"
  kubectl cluster-info --context "kind-${CLUSTER_NAME}" || true
  exit 0
fi

# Create cluster
kind create cluster --config "${SCRIPT_DIR}/cluster-config.yaml"

# Wait for control plane
echo "Waiting for control plane..."
kubectl wait --for=condition=Ready nodes --all --timeout=120s

echo "=== Cluster ${CLUSTER_NAME} ready ==="
kubectl cluster-info --context "kind-${CLUSTER_NAME}"
