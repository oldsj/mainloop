#!/usr/bin/env bash
# Builds the mock model image from fixture.json, loads it into kind-substrate-1-0 and rolls it out.
# The tag follows a hash of the sources, so a changed fixture rolls the Deployment.
set -euo pipefail
dir="$(cd "$(dirname "$0")" && pwd)"
tag="$(cat "$dir/fixture.json" "$dir/Dockerfile" "$dir/logproxy/main.go" | shasum -a 256 | cut -c1-12)"
docker buildx build --load --platform linux/arm64 -t "mainloop-mock-model:$tag" "$dir"
kind load docker-image --name substrate-1-0 "mainloop-mock-model:$tag"
sed "s/IMAGE_TAG/$tag/" "$dir/mock-model.yaml" | kubectl --context kind-substrate-1-0 apply -f -
kubectl --context kind-substrate-1-0 -n kagent rollout status deploy/mainloop-mock-model --timeout=5m
