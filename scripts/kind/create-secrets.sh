#!/usr/bin/env bash
# Create mainloop-secrets with random local values. An existing Secret is kept: a new database
# password would not match the PostgreSQL volume that already holds the old one.
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"
CONTEXT="kind-${CLUSTER_NAME}"

kubectl --context "${CONTEXT}" create namespace mainloop --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -

if kubectl --context "${CONTEXT}" get secret mainloop-secrets -n mainloop >/dev/null 2>&1; then
  echo "mainloop-secrets already exists; keeping it"
  exit 0
fi

echo "=== Creating mainloop-secrets with random local values ==="
db_password="$(openssl rand -hex 24)"
agent_token_key="$(openssl rand -hex 32)"
# The manifest goes to kubectl on stdin, so the values never appear in a process argument list.
kubectl --context "${CONTEXT}" apply -f - <<SECRET
apiVersion: v1
kind: Secret
metadata:
  name: mainloop-secrets
  namespace: mainloop
type: Opaque
stringData:
  db-username: mainloop
  db-password: ${db_password}
  agent-token-key: ${agent_token_key}
  # Empty on purpose: without a GitHub App the server keeps git transport and pushes off.
  github-app-id: ""
  github-app-private-key: ""
SECRET
echo "=== Secrets created ==="
