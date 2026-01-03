#!/usr/bin/env bash
# Create K8s secrets from .env file
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENV_FILE="${REPO_ROOT}/.env"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"

echo "=== Creating secrets from .env file ==="

if [[ ! -f "$ENV_FILE" ]]; then
    echo "ERROR: .env file not found at $ENV_FILE"
    echo "Copy .env.example to .env and configure it first"
    exit 1
fi

# Source the .env file
set -a
source "$ENV_FILE"
set +a

# Create mainloop namespace if not exists
kubectl --context="${KIND_CONTEXT}" create namespace mainloop --dry-run=client -o yaml | kubectl --context="${KIND_CONTEXT}" apply -f -

# Create claude-credentials secret (for agent-controller)
echo "Creating claude-credentials..."
kubectl --context="${KIND_CONTEXT}" create secret generic claude-credentials \
    --namespace mainloop \
    --from-literal=oauth-token="${CLAUDE_CODE_OAUTH_TOKEN:-}" \
    --dry-run=client -o yaml | kubectl --context="${KIND_CONTEXT}" apply -f -

# Create mainloop-secrets secret (for backend)
echo "Creating mainloop-secrets..."
kubectl --context="${KIND_CONTEXT}" create secret generic mainloop-secrets \
    --namespace mainloop \
    --from-literal=claude-secret-token="${CLAUDE_CODE_OAUTH_TOKEN:-}" \
    --from-literal=github-token="${GITHUB_TOKEN:-}" \
    --dry-run=client -o yaml | kubectl --context="${KIND_CONTEXT}" apply -f -

# Create ghcr-secret (optional - for pulling from GHCR if needed)
if [[ -n "${GHCR_TOKEN:-}" ]]; then
    echo "Creating ghcr-secret..."
    kubectl --context="${KIND_CONTEXT}" create secret docker-registry ghcr-secret \
        --namespace mainloop \
        --docker-server=ghcr.io \
        --docker-username="${GHCR_USER:-}" \
        --docker-password="${GHCR_TOKEN}" \
        --dry-run=client -o yaml | kubectl --context="${KIND_CONTEXT}" apply -f -
else
    echo "Skipping ghcr-secret (GHCR_TOKEN not set - using local images)"
fi

echo "=== Secrets created ==="
kubectl --context="${KIND_CONTEXT}" get secrets -n mainloop
