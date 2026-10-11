#!/usr/bin/env bash
# Refresh server/third_party/kagentapi from the oldsj/kagent fork.
# Usage: scripts/sync-kagent-api.sh <commit> [fork-checkout]
# Reads files with `git show`; never checks out or modifies the fork.
set -euo pipefail

commit="${1:?usage: sync-kagent-api.sh <commit> [fork-checkout]}"
fork="${2:-${KAGENT_FORK:-${HOME}/dev/kagent}}"
dest="$(cd "$(dirname "$0")/.." && pwd)/server/third_party/kagentapi"
src=go/api/gen/kagent/api/v1alpha1

# Message files only (no *_grpc.pb.go). sessions, agents and scheduled runs
# import common and runtime from the same package.
files=(common runtime sessions agents scheduled_runs)

full="$(git -C "the fork checkout" rev-parse --verify "${commit}^{commit}")"
for f in "${files[@]}"; do
  git -C "the fork checkout" show "${full}:${src}/${f}.pb.go" >"${dest}/${f}.pb.go"
done

cat >"${dest}/SOURCE" <<EOS
repo: oldsj/kagent
commit: ${full}
branch: mainloop-substrate-0-4
paths: ${src}/{$(
  IFS=,
  echo "${files[*]/%/.pb.go}"
)}
notes: message files copied verbatim; gRPC stubs are not copied. Refresh with scripts/sync-kagent-api.sh <commit>.
EOS
echo "synced ${#files[@]} files from ${full}"
