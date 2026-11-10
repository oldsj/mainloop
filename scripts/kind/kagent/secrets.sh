#!/usr/bin/env bash
# Kind only: the three kagent database Secrets, shaped like the prod 1Password items
# (kagent-postgres-owner, and the two items bootstrap-substrate-1-0-secrets.sh makes).
# Leaves existing Secrets alone.
set -euo pipefail
k=(kubectl --context=kind-substrate-1-0 -n kagent)
host="kagent-postgres-rw.kagent.svc:5432"
tls="sslmode=verify-full&sslrootcert=/run/kagent-postgres-ca/ca.crt"

make() { # secret user dsn-key
  if "${k[@]}" get secret "$1" >/dev/null 2>&1; then
    echo "$1 exists"
    return
  fi
  local pw
  pw="$(openssl rand -hex 24)"
  "${k[@]}" create secret generic "$1" \
    --from-literal=username="$2" \
    --from-literal=password="$pw" \
    --from-literal="$3"="postgres://$2:${pw}@${host}/kagent?${tls}" >/dev/null
  "${k[@]}" label secret "$1" experiment=substrate-upgrade >/dev/null
  echo "$1 created"
}
make kagent-postgres-owner kagent uri
make kagent-postgres-schema-owner kagent_owner_user ownerConnectionString
make kagent-postgres-readwrite kagent_readwrite_user readWriteConnectionString
