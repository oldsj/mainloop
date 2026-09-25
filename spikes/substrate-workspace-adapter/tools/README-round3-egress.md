# Round 3 egress helpers

These source files are live-only Substrate preview scaffolding, not production
credential-provider code.

Run these from the pinned Substrate checkout. Credential injection requires the Envoy
dataplane; `ate-setup` rejects the injection flag with agentgateway. The `0f9635ae`
preview used Envoy with sdsmint and both credential-provider overrides:

```sh
cd "$SUBSTRATE_SRC"
export VERSION=0f9635ae
export KO_DOCKER_REPO=localhost:5001
KUBECONFIG_PATH=/tmp/substrate-preview-kubeconfig

go run ./cmd/ate-setup --kind --kubeconfig "$KUBECONFIG_PATH" --context kind-substrate-preview \
  --atenet-dataplane envoy --experimental-use-sdsmint --experimental-egress-credential-injection \
  --credential-provider-name ate-secret://kubernetes.io \
  --credential-provider-address credprovider.mainloop-control.svc:50051 \
  deploy ate-system
go run ./cmd/ate-setup --kind --kubeconfig "$KUBECONFIG_PATH" --context kind-substrate-preview \
  --atenet-dataplane envoy --experimental-use-sdsmint --experimental-egress-credential-injection \
  --credential-provider-name ate-secret://kubernetes.io \
  --credential-provider-address credprovider.mainloop-control.svc:50051 \
  deploy atenet
```

At fork commit `0f9635ae`, the defaults are `ate-secret://k8s.io` and
`k8s-credential-provider.ate-system.svc:50051`. A redeploy without the overrides
breaks Claude's provider preflight with HTTP 500:
`credential URI names a provider this gateway does not serve`.

- `round3-credprovider/main.go` implements a temporary mTLS gRPC credential
  provider and HTTPS echo endpoint. It reads only a Kubernetes Secret created
  specifically for this test. The echo endpoint compares the injected header
  to that dummy value and returns a boolean result; it never returns the value.
- `round3-egress-injection/main.go` updates one actor's egress policy to inject
  a credential for one hostname. `--prefix` supports bearer-token headers;
  omit it for the dummy echo check.
- `round3-claude-provider/main.go` is the separate Phase 3e preview provider.
  It keeps the Claude Secret URI pinned, requires the expected actor SPIFFE ID
  through `EXPECTED_ACTOR_SPIFFE_ID`, reads the credential from a read-only
  Secret mount, and logs only a success marker. It is test-run scaffolding, not
  a general-purpose or production provider.

The `patched-next` adapter follows the current API shape: actor template UIDs are
read from `status.externalSnapshot.actorTemplateUid`; ActorTemplates use
`wakeupProbe` and `snapshotConfig`; and egress-policy updates carry the current
`metadata.uid` and `metadata.version` preconditions. The API has no egress-policy
delete operation, so the Claude proof revokes injected credentials by updating
the policy to an empty rule set.

Build from the pinned Substrate checkout (`0f9635aed37bd5dde604a9bca1975421cd07181a`),
where the imported internal packages and protobuf modules are available:

```sh
CGO_ENABLED=0 GOFLAGS=-mod=vendor go build -o /tmp/round3-credprovider ./cmd/round3-credprovider
CGO_ENABLED=0 GOFLAGS=-mod=vendor go build -o /tmp/round3-egress-injection ./cmd/round3-egress-injection
```

## Build and push the Lane A Claude provider image

Run this block from the Mainloop worktree root with access to pull the builder
and distroless images and push to the local registry. It makes a temporary
checkout of Substrate at `0f9635ae`, stages this provider source and Dockerfile
there, builds a static nonroot image, pushes a unique tag, then reads the digest
from the registry. The expected actor SPIFFE ID is supplied at runtime by the
Lane A Deployment, not baked into this image.

```sh
set -euo pipefail
MAINLOOP_ROOT=$(git rev-parse --show-toplevel)
BUILD_ROOT=$(mktemp -d /tmp/round3-claude-provider-build.XXXXXX)
trap 'rm -rf "$BUILD_ROOT"' EXIT
: "${SUBSTRATE_SRC:?set SUBSTRATE_SRC to the pinned Substrate checkout path}"
SUBSTRATE_TMP=$BUILD_ROOT/substrate
SUBSTRATE_COMMIT=0f9635aed37bd5dde604a9bca1975421cd07181a
IMAGE_TAG=0f9635ae-lane-a
IMAGE=localhost:5001/round3-claude-provider:$IMAGE_TAG

mkdir -p "$SUBSTRATE_TMP"
test "$(git -C "$SUBSTRATE_SRC" rev-parse "$SUBSTRATE_COMMIT^{commit}")" = "$SUBSTRATE_COMMIT"
git -C "$SUBSTRATE_SRC" archive "$SUBSTRATE_COMMIT" | tar -x -C "$SUBSTRATE_TMP"
mkdir -p "$SUBSTRATE_TMP/tools/round3-claude-provider"
cp "$MAINLOOP_ROOT/spikes/substrate-workspace-adapter/tools/round3-claude-provider/main.go" \
  "$SUBSTRATE_TMP/tools/round3-claude-provider/main.go"
cp "$MAINLOOP_ROOT/spikes/substrate-workspace-adapter/tools/round3-claude-provider/Dockerfile" \
  "$SUBSTRATE_TMP/tools/round3-claude-provider/Dockerfile"

cd "$SUBSTRATE_TMP"
docker build --pull --platform=linux/amd64 \
  -f tools/round3-claude-provider/Dockerfile \
  -t "$IMAGE" .
docker push "$IMAGE"

DIGEST=$(curl --fail --silent --show-error --head --max-time 15 \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "http://localhost:5001/v2/round3-claude-provider/manifests/$IMAGE_TAG" |
  awk 'tolower($1) == "docker-content-digest:" { gsub("\r", "", $2); print $2; exit }')
[[ $DIGEST =~ ^sha256:[0-9a-f]{64}$ ]]
printf 'CLAUDE_PROVIDER_IMAGE=localhost:5001/round3-claude-provider@%s\n' "$DIGEST"
```

Use only a throwaway dummy Secret with the first provider in a disposable
preview cluster. Set `EXPECTED_ACTOR_SPIFFE_ID` in the provider Deployment to
the one authorized actor ID. Pass the provider's credential only as a read-only
Secret mount. Never put a credential in source, logs, actor commands, or echo
responses.
