#!/usr/bin/env bash
# Install Substrate v1.0.1 on the Kind cluster: PostgreSQL, the four Helm releases through
# `kubectl ate install`, and one gVisor worker pool. Safe to rerun.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"
CONTEXT="kind-${CLUSTER_NAME}"
SUBSTRATE_VERSION="v1.0.1"
RELEASES="https://github.com/kagent-dev/substrate/releases/download/${SUBSTRATE_VERSION}"
SETUP_SQL_URL="https://raw.githubusercontent.com/kagent-dev/substrate/${SUBSTRATE_VERSION}/pkg/postgressetup/setup.sql"
SETUP_SQL_SHA256="609aa9900dcee152a9e8cc75b9fd6ec57f37e70ed9dfd73c820cf2e2bf310a09"
CACHE="${REPO_ROOT}/.cache/substrate/${SUBSTRATE_VERSION}"

k() { kubectl --context "${CONTEXT}" "$@"; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fetch() { # url sha256 destination
  if [[ -f $3 && "$(sha256_of "$3")" == "$2" ]]; then return; fi
  curl -fsSL --retry 3 -o "$3.tmp" "$1"
  if [[ "$(sha256_of "$3.tmp")" != "$2" ]]; then
    echo "ERROR: $1 does not match sha256 $2" >&2
    rm -f "$3.tmp"
    exit 1
  fi
  mv "$3.tmp" "$3"
}

case "$(uname -s)-$(uname -m)" in
Darwin-arm64) ATE_ASSET=kubectl-ate-darwin-arm64 ATE_SHA256=43e59f0397b7755607207aa823d84f3913aacb59884ce96730955a9b1a286935 ;;
Darwin-x86_64) ATE_ASSET=kubectl-ate-darwin-amd64 ATE_SHA256=9e148631e6186726d13118050b0a8605899b06a25e410ec73619372d30b1bfc7 ;;
Linux-x86_64) ATE_ASSET=kubectl-ate-linux-amd64 ATE_SHA256=455a3c465e9473575083cb54d5646eac0ba6f299f8e1b7e42077a3997c17afaa ;;
Linux-aarch64 | Linux-arm64) ATE_ASSET=kubectl-ate-linux-arm64 ATE_SHA256=e3973b4fffd1a509beec322cae4bdfc63b9dc9057bcd9ce858565bb9436d5b32 ;;
*)
  echo "ERROR: no kubectl-ate build for $(uname -s)-$(uname -m)" >&2
  exit 1
  ;;
esac

mkdir -p "${CACHE}"
ATE="${CACHE}/kubectl-ate"
fetch "${RELEASES}/${ATE_ASSET}" "${ATE_SHA256}" "${ATE}"
chmod +x "${ATE}"
fetch "${SETUP_SQL_URL}" "${SETUP_SQL_SHA256}" "${CACHE}/setup.sql"

echo "=== PostgreSQL for Substrate ==="
for ns in substrate-db ate-system; do
  k create namespace "${ns}" --dry-run=client -o yaml | k apply -f -
done
# atelet mounts host paths, so its namespace cannot enforce a stricter Pod Security level.
k label namespace ate-system pod-security.kubernetes.io/enforce=privileged --overwrite
# The identity `kubectl ate proxy actor` presents to the router. The chart does not create it.
k create serviceaccount atenet-client -n ate-system --dry-run=client -o yaml | k apply -f -

# The passwords are kept once created: new ones would not match the roles in the database volume.
if ! k get secret substrate-postgres-admin -n substrate-db >/dev/null 2>&1; then
  k apply -f - <<SECRET
apiVersion: v1
kind: Secret
metadata:
  name: substrate-postgres-admin
  namespace: substrate-db
type: Opaque
stringData:
  password: $(openssl rand -hex 24)
  owner-password: $(openssl rand -hex 24)
  readwrite-password: $(openssl rand -hex 24)
SECRET
fi
k apply -f "${SCRIPT_DIR}/postgres.yaml"
k rollout status statefulset/substrate-postgres -n substrate-db --timeout=180s

secret_value() { k get secret substrate-postgres-admin -n substrate-db -o "jsonpath={.data.$1}" | base64 -d; }
owner_password="$(secret_value owner-password)"
readwrite_password="$(secret_value readwrite-password)"

# setup.sql creates the roles, the logins and the schema, and is idempotent. It keeps its inputs in
# transaction-local settings, so it needs a single transaction (-1). The passwords go to
# psql on stdin, so they never appear in a process argument list.
{
  printf '\\set substrate_schema substrate\n'
  printf '\\set substrate_owner_role substrate_owner\n'
  printf '\\set substrate_owner_user substrate_owner_user\n'
  printf '\\set substrate_owner_password %s\n' "${owner_password}"
  printf '\\set substrate_readwrite_role substrate_readwrite\n'
  printf '\\set substrate_readwrite_user substrate_readwrite_user\n'
  printf '\\set substrate_readwrite_password %s\n' "${readwrite_password}"
  cat "${CACHE}/setup.sql"
} | k exec -i -n substrate-db substrate-postgres-0 -- psql -v ON_ERROR_STOP=1 -q -1 -U postgres -d substrate

host="substrate-postgres.substrate-db.svc:5432"
k apply -f - <<SECRET
apiVersion: v1
kind: Secret
metadata:
  name: substrate-postgres-owner
  namespace: ate-system
type: Opaque
stringData:
  ownerConnectionString: postgres://substrate_owner_user:${owner_password}@${host}/substrate?sslmode=disable
---
apiVersion: v1
kind: Secret
metadata:
  name: substrate-postgres-readwrite
  namespace: ate-system
type: Opaque
stringData:
  readWriteConnectionString: postgres://substrate_readwrite_user:${readwrite_password}@${host}/substrate?sslmode=disable
SECRET

echo "=== Substrate ${SUBSTRATE_VERSION} ==="
"${ATE}" --context "${CONTEXT}" install --no-tui --config "${SCRIPT_DIR}/install-config.yaml"

echo "=== gVisor worker pool ==="
k apply -f "${SCRIPT_DIR}/workerpool.yaml"
k wait workerpool/gvisor -n substrate-demo --for=jsonpath='{.status.readyReplicas}'=2 --timeout=300s
"${ATE}" --context "${CONTEXT}" status
