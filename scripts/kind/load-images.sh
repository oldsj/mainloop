#!/usr/bin/env bash
# Build and load images into Kind cluster
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

echo "=== Building and loading images into Kind cluster ==="

cd "${REPO_ROOT}"

# Build images with test tag (shares cache with make dev)
echo "Building backend..."
docker build -f backend/Dockerfile -t mainloop-backend:test .

echo "Building frontend..."
docker build -f frontend/Dockerfile \
  --build-arg VITE_API_URL=http://localhost:8081 \
  -t mainloop-frontend:test .

echo "Building agent-controller..."
docker build -f claude-agent/Dockerfile \
  -t mainloop-agent-controller:test ./claude-agent

# Load images into Kind
echo "Loading images into Kind cluster..."
kind load docker-image mainloop-backend:test --name "${CLUSTER_NAME}"
kind load docker-image mainloop-frontend:test --name "${CLUSTER_NAME}"
kind load docker-image mainloop-agent-controller:test --name "${CLUSTER_NAME}"

echo "=== Images loaded ==="
docker exec "${CLUSTER_NAME}-control-plane" crictl images | grep mainloop || true
