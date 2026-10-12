FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 AS base

ARG TARGETARCH
# The package caches live outside $HOME and /data: workspace runtimes move HOME and
# XDG_CACHE_HOME onto the durable /data volume, which would hide an image-baked cache.
# Installs copy from them: hardlinking a file in a read-only image layer forces an
# overlay copy-up per file and measured far slower than a plain copy.
ENV HOME=/home/nonroot \
    XDG_CACHE_HOME=/home/nonroot/.cache \
    TRUNK_CACHE=/home/nonroot/.cache/trunk \
    GOTOOLCHAIN=local \
    GOMODCACHE=/opt/mainloop-cache/go-mod \
    GOCACHE=/opt/mainloop-cache/go-build \
    GOLANGCI_LINT_CACHE=/opt/mainloop-cache/golangci-lint \
    npm_config_store_dir=/opt/mainloop-cache/pnpm-store \
    npm_config_package_import_method=copy \
    PATH=/usr/local/go/bin:/usr/lib/postgresql/16/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

COPY .devcontainer/install-tools /usr/local/lib/mainloop/install-tools
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git make ripgrep \
       build-essential pkg-config libpq-dev xz-utils unzip gnupg locales util-linux \
    && /usr/local/lib/mainloop/install-tools "${TARGETARCH}" \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/nonroot --shell /bin/bash nonroot \
    && mkdir -p /data /workspace /home/nonroot/.cache /opt/mainloop-cache \
    && chown -R 65532:65532 /data /workspace /home/nonroot /opt/mainloop-cache

# Prewarm the pnpm store from the lockfile only. Application source stays out of the
# image; only the populated cache is copied into the final image.
FROM base AS deps
USER 65532:65532
WORKDIR /opt/mainloop-deps
COPY --chown=65532:65532 package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY scripts/with-timeout.mjs /usr/local/lib/mainloop/with-timeout.mjs
RUN node /usr/local/lib/mainloop/with-timeout.mjs 240 "pnpm prewarm" -- pnpm fetch --frozen-lockfile

# Prewarm the Go module and build caches from go.mod/go.sum only. Application source
# stays out of the image; std and every dependency package are compiled in both normal
# and race mode so a workspace's first `go build`/`go test -race` is a cache hit.
FROM base AS go-deps
USER 65532:65532
WORKDIR /opt/mainloop-go-deps
COPY --chown=65532:65532 server/go.mod server/go.sum ./
COPY scripts/with-timeout.mjs /usr/local/lib/mainloop/with-timeout.mjs
RUN node /usr/local/lib/mainloop/with-timeout.mjs 600 "go mod download" -- go mod download
RUN go build std
# race-std and every dependency package are best-effort cache warmup, not a
# correctness requirement: a single incidental failure (an OOM'd cgo helper, an
# unsupported arch/os combination) must not fail the whole image build.
RUN go build -race std || true
# Each dependency module builds in its own `go build` invocation. `go list -m all`
# includes modules kept only for version resolution, and bundling many patterns into
# one `go build <pat1> <pat2> ...` means a single unresolvable one (a stale example
# importing a path that moved, a Windows-only cmd, platform-specific asm) aborts
# resolution before anything compiles, caching nothing. Per-module invocations let
# every other module still warm its cache entry.
RUN go list -m -f '{{if not .Main}}{{.Path}}/...{{end}}' all | grep -v '^$' > /tmp/go-deps.txt \
    && xargs -P "$(nproc)" -I{} sh -c 'go build "{}" >/dev/null 2>&1 || true' < /tmp/go-deps.txt \
    && xargs -P "$(nproc)" -I{} sh -c 'go build -race "{}" >/dev/null 2>&1 || true' < /tmp/go-deps.txt

FROM base
# Only lint configuration and package caches enter the public image, never application
# source or secrets.
COPY .trunk/trunk.yaml /opt/mainloop-lint/.trunk/trunk.yaml
COPY .trunk/configs/ /opt/mainloop-lint/.trunk/configs/
COPY .devcontainer/dev-postgres /usr/local/bin/dev-postgres
COPY scripts/with-timeout.mjs /usr/local/lib/mainloop/with-timeout.mjs
WORKDIR /opt/mainloop-lint
RUN git init \
    && node /usr/local/lib/mainloop/with-timeout.mjs 300 'trunk install' -- trunk install --ci --no-progress \
    && find /home/nonroot/.cache/trunk/tools -type f -exec sha256sum {} + > /opt/mainloop-lint/tool-checksums.txt \
    && chown -R 65532:65532 /home/nonroot /opt/mainloop-lint

COPY --from=deps --chown=65532:65532 /opt/mainloop-cache /opt/mainloop-cache
COPY --from=go-deps --chown=65532:65532 /opt/mainloop-cache /opt/mainloop-cache

WORKDIR /workspace
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s CMD test -w /data && test -w "$HOME" && test -x /usr/lib/postgresql/16/bin/pg_ctl
CMD ["bash"]
