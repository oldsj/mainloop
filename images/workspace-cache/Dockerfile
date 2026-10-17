# syntax=docker/dockerfile:1
# Workspace cache image: the development image plus dependency and Go build caches for one commit
# of main, so a fresh workspace clone compiles nothing. Build context is a git checkout of that
# commit (with .git): docker buildx build -f images/workspace-cache/Dockerfile \
#   --build-arg BASE=ghcr.io/oldsj/mainloop-dev@sha256:<digest> --build-arg SOURCE_SHA=<sha> .
#
# Caches live under /opt/cache, outside /data, so Data-scope snapshots never carry them.
#
# Layer 1 (deps) depends only on go.mod/go.sum and the pnpm lockfile. Its mtimes are pinned, so a
# rebuild whose lock files are unchanged produces a bit-identical layer and nodes pull only layer 2.
# Layer 2 (build) is the Go build cache for the exact commit. Go keys compiled packages by
# directory (no -trimpath) and, for main packages, by VCS stamp (revision, time, modified), so the
# commit is built from a real git clone at /data/workspace, the path and shape of a session
# checkout. Only /opt/cache reaches the final image, never source.
# BASE is always passed by the build. Its fallback is a tag that does not exist, so a build
# without it fails at the pull instead of silently using a mutable base.
ARG BASE
FROM ${BASE:-ghcr.io/oldsj/mainloop-dev:pass-BASE-build-arg} AS base

FROM base AS prep
USER root
RUN mkdir -p /opt/cache /data/workspace && chown -R 65532:65532 /opt/cache /data/workspace
USER 65532:65532
ENV GOMODCACHE=/opt/cache/go-mod \
    GOCACHE=/opt/cache/go-build \
    npm_config_store_dir=/opt/cache/pnpm-store \
    npm_config_virtual_store_dir=/opt/cache/pnpm-virtual \
    npm_config_package_import_method=copy \
    CI=true
WORKDIR /data/workspace

FROM prep AS deps
# Fixed modification time for everything in layer 1 (2026-01-01T00:00:00Z). It is old enough
# never to matter and constant, unlike the extraction time the downloads would otherwise carry.
ARG DEPS_EPOCH=1767225600
COPY --chown=65532:65532 server/go.mod server/go.sum server/
COPY --chown=65532:65532 package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY --chown=65532:65532 web/package.json web/
# One RUN, so the file times are set on files this step created. A separate RUN would copy up every
# file in the overlay first, which is very slow for the module cache.
# fetch fills the content store; install --offline then builds the shared virtual store from it.
RUN (cd server && go mod download) \
    && pnpm fetch --frozen-lockfile \
    && pnpm install --frozen-lockfile --offline \
    && find /opt/cache/go-mod /opt/cache/pnpm-store /opt/cache/pnpm-virtual -exec touch -h -d "@${DEPS_EPOCH}" {} +

FROM prep AS build
ARG SOURCE_SHA
COPY --from=deps --chown=65532:65532 /opt/cache/go-mod /opt/cache/go-mod
COPY --from=deps --chown=65532:65532 /opt/cache/pnpm-store /opt/cache/pnpm-store
COPY --from=deps --chown=65532:65532 /opt/cache/pnpm-virtual /opt/cache/pnpm-virtual
# A real clone at the commit, as a session makes. safe.directory is needed because the mounted
# context is owned by another user. It goes in a throwaway global config file: git drops -c and
# GIT_CONFIG_COUNT for the helper process a local clone starts, but keeps GIT_CONFIG_GLOBAL. The shallow clone is enough for Go's VCS stamp.
RUN --mount=type=bind,target=/src \
    export GIT_CONFIG_GLOBAL="$(mktemp)" \
    && git config --global --add safe.directory '*' \
    && test -n "${SOURCE_SHA}" \
    && git clone --quiet --no-hardlinks --depth 1 file:///src . \
    && test "$(git rev-parse HEAD)" = "${SOURCE_SHA}" \
    && test -z "$(git status --porcelain)"
# Compile every package and every test binary; -exec /bin/true skips running tests (some need a
# database). The tree must still be clean afterwards, or the stamp would differ from a session's.
WORKDIR /data/workspace/server
RUN go build ./... && go test -exec /bin/true -run XXX_none ./... >/dev/null \
    && test -z "$(git status --porcelain)"

FROM base AS cache
ARG SOURCE_SHA
LABEL org.opencontainers.image.revision=${SOURCE_SHA}
USER root
# Layer 1: dependencies (module cache, pnpm content store, shared virtual store).
COPY --from=deps --chown=65532:65532 /opt/cache/go-mod /opt/cache/go-mod
COPY --from=deps --chown=65532:65532 /opt/cache/pnpm-store /opt/cache/pnpm-store
COPY --from=deps --chown=65532:65532 /opt/cache/pnpm-virtual /opt/cache/pnpm-virtual
# Layer 2: build outputs.
COPY --from=build --chown=65532:65532 /opt/cache/go-build /opt/cache/go-build
USER 65532:65532
ENV GOMODCACHE=/opt/cache/go-mod \
    GOCACHE=/opt/cache/go-build \
    npm_config_store_dir=/opt/cache/pnpm-store \
    npm_config_virtual_store_dir=/opt/cache/pnpm-virtual
# Replaces the base image's check with one for what this image adds.
HEALTHCHECK --interval=30s --timeout=3s CMD test -r /opt/cache/go-build && test -r /opt/cache/go-mod
