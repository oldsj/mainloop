#!/usr/bin/env bash
# Builds the kagent controller from a local kagent checkout and loads it into
# kind-substrate-1-0, so a branch can be tested before its images publish.
# Nothing is pushed to a registry. Usage: load-local.sh [KAGENT_CHECKOUT]
#
# Only the controller can be loaded this way. Actor images (harness, runtime
# payload) are pulled by atelet straight from a registry, not from the kind
# node's containerd.
set -euo pipefail
src="$(cd "${1:-.}" && pwd)"
cluster=substrate-1-0
[[ -f "$src/go/Dockerfile" ]] || { echo "$src is not a kagent checkout" >&2; exit 1; }

tag="local-$(git -C "$src" rev-parse --short=7 HEAD)"
if [[ -n "$(git -C "$src" status --porcelain)" ]]; then
  tag="$tag-dirty-$(date +%s)"
fi
image="ghcr.io/oldsj/kagent-controller:$tag"

# The build args of .github/workflows/mainloop-fork-images.yaml.
docker buildx build --load --platform linux/arm64 \
  -f "$src/go/Dockerfile" -t "$image" \
  --build-arg VERSION="$tag" \
  --build-arg DOCKER_REPO=kagent-dev/kagent \
  --build-arg DOCKER_REGISTRY=ghcr.io/oldsj \
  --build-arg BASE_IMAGE_REGISTRY=cgr.dev \
  --build-arg TOOLS_UV_VERSION=0.10.4 \
  --build-arg TOOLS_PYTHON_VERSION=3.13 \
  --build-arg TOOLS_NODE_VERSION=24 \
  --build-arg LDFLAGS="-X github.com/kagent-dev/kagent/go/core/internal/version.Version=$tag" \
  --build-arg BUILD_PACKAGE=./core/cmd/controller \
  "$src/go"
kind load docker-image --name "$cluster" "$image"

echo "Loaded $image. The kind owner applies it with:"
echo "  $(dirname "$0")/upgrade.sh --set controller.image.tag=$tag"
