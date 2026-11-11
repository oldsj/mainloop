#!/usr/bin/env bash
# Upgrades the fork kagent on kind-substrate-1-0 with the prod-shaped values. SESSION_POLICY=off
# leaves out session-policy.yaml, for E2E suites that create Agents with generated names.
# VERSION defaults to the chart the values were written for. Extra arguments go to helm
# upgrade kagent, such as the --set that load-local.sh prints.
set -euo pipefail
dir="$(cd "$(dirname "$0")" && pwd)"
version="${VERSION:-0.0.0-7988e65}"
ctx=kind-substrate-1-0
policy=(-f "$dir/session-policy.yaml")
[[ "${SESSION_POLICY:-on}" == off ]] && policy=()
helm --kube-context "$ctx" upgrade kagent-crds oci://ghcr.io/oldsj/charts/kagent-crds --version "$version" -n kagent --wait
helm --kube-context "$ctx" upgrade kagent oci://ghcr.io/oldsj/charts/kagent --version "$version" -n kagent \
  -f "$dir/values.yaml" -f "$dir/values-prod-shape.yaml" "${policy[@]}" "$@"
kubectl --context "$ctx" -n kagent rollout status deploy/kagent-controller --timeout=10m
