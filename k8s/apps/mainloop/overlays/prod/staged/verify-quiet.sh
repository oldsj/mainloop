#!/bin/sh
# Read-only production gate. Run only within an approved rollout stage.
set -eu
case "${1:-}" in
  on|off) expected_hibernation=$1 ;;
  *) echo 'usage: sh verify-quiet.sh on|off' >&2; exit 1 ;;
esac
k() { kubectl --context=admin@internal-01 --request-timeout=10s -n mainloop "$@"; }
check_configuration() {
  if ! selector=$(k get deployment mainloop-backend -o jsonpath='{.spec.selector.matchLabels}'); then exit 1; fi
  # A changed selector needs a reviewed gate, not an empty-result assumption.
  test "$selector" = '{"app":"mainloop-backend"}'
  if ! replicas=$(k get deployment mainloop-backend -o jsonpath='{.spec.replicas}'); then exit 1; fi
  test "$replicas" = 0
  if ! hibernation=$(k get cluster mainloop-db -o jsonpath='{.metadata.annotations.cnpg\.io/hibernation}'); then exit 1; fi
  test "$hibernation" = "$expected_hibernation"
}
check_configuration
attempt=0
while :; do
  # Any API/read error fails closed. Terminating Pods are still present.
  if ! pods=$(k get pods -l app=mainloop-backend -o name); then exit 1; fi
  if [ -z "$pods" ]; then break; fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    echo 'backend termination deadline exceeded' >&2
    exit 1
  fi
  sleep 5
done
check_configuration
if ! pods=$(k get pods -l app=mainloop-backend -o name); then exit 1; fi
test -z "$pods"
echo 'backend desired replicas zero and no matching Pods; hibernation matches'
