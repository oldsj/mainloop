#!/usr/bin/env bash
# Create the local Kind cluster if it does not exist yet.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo "Cluster ${CLUSTER_NAME} already exists"
  exit 0
fi

echo "=== Creating Kind cluster: ${CLUSTER_NAME} ==="
kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/cluster-config.yaml"
kubectl --context "kind-${CLUSTER_NAME}" wait --for=condition=Ready nodes --all --timeout=120s
echo "=== Cluster ${CLUSTER_NAME} ready ==="
