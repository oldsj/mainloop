#!/bin/sh
# Read-only production gate. No Secret values or workload mutation.
set -eu
k() { kubectl --context=admin@internal-01 --request-timeout=10s "$@"; }
if ! phase=$(k -n mainloop get pvc mainloop-feature-archive-20261005 -o jsonpath='{.status.phase}'); then exit 1; fi
test "$phase" = Bound
if ! pv=$(k -n mainloop get pvc mainloop-feature-archive-20261005 -o jsonpath='{.spec.volumeName}'); then exit 1; fi
test -n "$pv"
if ! retention=$(k get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}'); then exit 1; fi
test "$retention" = Retain
if ! storage=$(k get pv "$pv" -o jsonpath='{.spec.storageClassName}'); then exit 1; fi
test "$storage" = synology-ssd
if ! claim=$(k get pv "$pv" -o jsonpath='{.spec.claimRef.namespace}/{.spec.claimRef.name}'); then exit 1; fi
test "$claim" = mainloop/mainloop-feature-archive-20261005
printf 'verified retained archive PV: %s\n' "$pv"
