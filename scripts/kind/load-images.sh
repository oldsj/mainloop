#!/usr/bin/env bash
# Build the server and web images from this checkout and load them into the Kind cluster.
# `make dev` does not use this script: DevSpace builds and loads its own images. This is the path
# for `make kind-up` and `make kind-load`, which deploy without DevSpace.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"

# The web bundle bakes in its API URL at build time. It must match the API port forward in
# docs/dev-environment.md#kind.
API_URL=http://localhost:8000

echo "=== Building images ==="
docker build --target prod -t mainloop-backend:kind "${REPO_ROOT}/server"
docker build -f "${REPO_ROOT}/web/Dockerfile" --build-arg "VITE_API_URL=${API_URL}" -t mainloop-frontend:kind "${REPO_ROOT}"

echo "=== Loading images into kind-${CLUSTER_NAME} ==="
kind load docker-image mainloop-backend:kind mainloop-frontend:kind --name "${CLUSTER_NAME}"
echo "=== Images loaded ==="
