#!/usr/bin/env bash
# Upgrades the fork kagent on kind-substrate-1-0. Pass extra values files to layer on top,
# such as no-session-policy.yaml. VERSION defaults to the chart the values were written for.
set -euo pipefail
dir="$(cd "$(dirname "$0")" && pwd)"
version="${VERSION:-0.0.0-42d39d4}"
ctx=kind-substrate-1-0
extra=()
for f in "$@"; do extra+=(-f "$f"); done
helm --kube-context "$ctx" upgrade kagent-crds oci://ghcr.io/oldsj/charts/kagent-crds --version "$version" -n kagent --wait
helm --kube-context "$ctx" upgrade kagent oci://ghcr.io/oldsj/charts/kagent --version "$version" -n kagent \
  -f "$dir/values.yaml" -f "$dir/values-prod-shape.yaml" "${extra[@]}"
kubectl --context "$ctx" -n kagent rollout status deploy/kagent-controller --timeout=10m
