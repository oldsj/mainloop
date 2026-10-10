#!/usr/bin/env bash
# Dev kagent harness images for mainloop-dev (`make dev-harness`).
#
#   dev-live-harness.sh build        build arm64 Claude and Codex harness images from the
#                                    kagent fork, push dev-<sha> tags to ghcr, record them
#                                    and apply the *-workspace-dev agents with them
#   dev-live-harness.sh image <p>    print the recorded image for claude|codex, else the
#                                    production pin from the dev manifests (DevSpace var)
#   dev-live-harness.sh reset        forget recorded images (next `make dev-live` uses the pins)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
STATE="$ROOT/.devspace/dev-harness"
MANIFESTS="$ROOT/k8s/apps/mainloop/overlays/dev-live/kagent"
KAGENT_DIR=${KAGENT_DIR:-$HOME/dev/kagent}
REGISTRY=ghcr.io
REPO=oldsj

image() {
  local provider=$1
  if [[ -s "$STATE/$provider" ]]; then
    cat "$STATE/$provider"
  else
    sed -n 's/^ *image: *\(.*\)$/\1/p' "$MANIFESTS/$provider.yaml" | head -1
  fi
}

build() {
  [[ -d "$KAGENT_DIR/.git" ]] || { echo "kagent fork not found at $KAGENT_DIR" >&2; exit 1; }
  local sha tag
  sha=$(git -C "$KAGENT_DIR" rev-parse --short=7 HEAD)
  tag="dev-$sha"
  if [[ -n "$(git -C "$KAGENT_DIR" status --porcelain)" ]]; then
    tag="$tag-dirty-$(date +%s)"
  fi
  echo "Building arm64 harness images $tag from $KAGENT_DIR"
  make -C "$KAGENT_DIR" build-claude-harness build-codex-harness \
    LOCALARCH=arm64 DOCKER_REGISTRY="$REGISTRY" DOCKER_REPO="$REPO" \
    CLAUDE_HARNESS_IMAGE_NAME=kagent-claude-harness CLAUDE_HARNESS_IMAGE_TAG="$tag" \
    CODEX_HARNESS_IMAGE_NAME=kagent-codex-harness CODEX_HARNESS_IMAGE_TAG="$tag"

  mkdir -p "$STATE"
  local provider ref digest
  for provider in claude codex; do
    ref="$REGISTRY/$REPO/kagent-$provider-harness:$tag"
    digest=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}' | jq -er .digest)
    echo "$ref@$digest" > "$STATE/$provider"
    echo "$provider-workspace-dev-harness -> $ref@$digest"
  done
  "$ROOT/scripts/dev-live.sh" agents
}

case "${1:-}" in
  image) image "${2:?provider}" ;;
  build) build ;;
  reset) rm -rf "$STATE" ;;
  *) sed -n '2,10p' "$0" >&2; exit 2 ;;
esac
