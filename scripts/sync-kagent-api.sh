#!/usr/bin/env bash
# Refresh server/third_party/kagentapi from the oldsj/kagent fork.
# Usage: scripts/sync-kagent-api.sh <commit> [fork-checkout]
# Reads files with `git show`; never checks out or modifies the fork. Files are staged in a
# directory beside the destination and moved in only after every one was read, so a missing
# upstream file aborts before anything is overwritten.
set -euo pipefail

commit="${1:?usage: sync-kagent-api.sh <commit> [fork-checkout]}"
fork="${2:-${KAGENT_FORK:-${HOME}/dev/kagent}}"
dest="$(cd "$(dirname "$0")/.." && pwd)/server/third_party/kagentapi"
src=go/api/gen/kagent/api/v1alpha1

# Message files only (no *_grpc.pb.go). sessions, agents and scheduled runs
# import common and runtime from the same package.
files=(common runtime sessions agents scheduled_runs)

full="$(git -C "${fork}" rev-parse --verify "${commit}^{commit}")"

stage="$(mktemp -d "${dest}/.sync.XXXXXX")"
trap 'rm -rf "${stage}"' EXIT

for f in "${files[@]}"; do
  git -C "${fork}" show "${full}:${src}/${f}.pb.go" >"${stage}/${f}.pb.go"
done

cat >"${stage}/SOURCE" <<EOS
repo: oldsj/kagent
commit: ${full}
paths: ${src}/{$(
  IFS=,
  echo "${files[*]/%/.pb.go}"
)}
notes: message files copied verbatim; gRPC stubs are not copied. Refresh with scripts/sync-kagent-api.sh <commit>.
EOS

for f in "${files[@]/%/.pb.go}" SOURCE; do
  mv "${stage}/${f}" "${dest}/${f}"
done
echo "synced ${#files[@]} files from ${full}"
