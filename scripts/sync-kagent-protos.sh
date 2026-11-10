#!/usr/bin/env bash
# Re-vendor kagent's control API protos from an oldsj/kagent checkout at one commit, re-apply the
# overlay of fork additions that commit does not carry yet, and regenerate the Go clients.
#
#   scripts/sync-kagent-protos.sh <path to oldsj/kagent checkout> <commit>
#
# Afterwards: update `commit:` in server/third_party/kagentproto/SOURCE and deployedSourceRevision in
# source_test.go. When the commit carries part of the overlay, the patch fails on those hunks:
# remove them from overlay.patch (and their entry in SOURCE) and run the script again.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <kagent checkout> <commit>" >&2
  exit 2
fi
fork=$1
commit=$2
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor="${root}/server/third_party/kagentproto"
dest="${vendor}/kagent/api/v1"

for f in common runtime sessions agents scheduled_runs checkpoints; do
  git -C "${fork}" show "${commit}:proto/kagent/api/v1/${f}.proto" >"${dest}/${f}.proto"
done
if [[ -s ${vendor}/overlay.patch ]]; then
  patch --forward --no-backup-if-mismatch -p1 -d "${vendor}" <"${vendor}/overlay.patch"
fi
(cd "${root}/server" && go generate ./third_party/kagentproto/...)
echo "vendored ${commit}; now update SOURCE and source_test.go"
