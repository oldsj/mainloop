# trunk-ignore-all(trivy/DS-0026): Kubernetes probes /health; the image has no HTTP client for a HEALTHCHECK
# checkov:skip=CKV_DOCKER_2: Kubernetes probes /health; the image has no HTTP client to run a HEALTHCHECK
# Build context is this directory: docker build --target prod server
ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# REVISION is the commit the image is built from; /health reports it.
ARG REVISION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/oldsj/mainloop/server/internal/buildinfo.Revision=${REVISION}" -o /out/mainloop ./cmd/mainloop

# kubectl for serve-executor's command.kubectl, pinned to the cluster's minor version and checked
# against the release's published sha256 for each architecture.
FROM golang:${GO_VERSION}-bookworm AS kubectl
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.37.2
ARG KUBECTL_SHA256_AMD64=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
ARG KUBECTL_SHA256_ARM64=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
RUN arch="${TARGETARCH:-amd64}" \
    && sum="$(case "${arch}" in arm64) echo "${KUBECTL_SHA256_ARM64}";; amd64) echo "${KUBECTL_SHA256_AMD64}";; esac)" \
    && test -n "${sum}" \
    && curl -fsSLo /kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" \
    && echo "${sum}  /kubectl" | sha256sum -c - \
    && chmod 0555 /kubectl

# prod: git for the git transport, util-linux for the workspace helpers, CA certificates for
# TLS to GitHub and kagent, kubectl for serve-executor.
FROM debian:bookworm-slim AS prod
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 1000 --create-home --shell /usr/sbin/nologin mainloop
COPY --from=build /out/mainloop /usr/local/bin/mainloop
COPY --from=kubectl /kubectl /usr/local/bin/kubectl
USER 1000:1000
EXPOSE 8000 8002 8003 8004
ENTRYPOINT ["/usr/local/bin/mainloop"]
CMD ["serve-api"]

# dev: Go toolchain with watchexec to rebuild on change. The source is baked in at /src so the
# pod starts and migrates before any sync; DevSpace then syncs the working tree over it. Runs as
# uid 1000 like the prod image, with a writable module and build cache.
FROM golang:${GO_VERSION}-bookworm AS dev
ARG TARGETARCH
ARG WATCHEXEC_VERSION=2.8.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends git util-linux xz-utils \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && arch="$(case "${TARGETARCH:-amd64}" in arm64) echo aarch64;; *) echo x86_64;; esac)" \
    && curl -fsSL "https://github.com/watchexec/watchexec/releases/download/v${WATCHEXEC_VERSION}/watchexec-${WATCHEXEC_VERSION}-${arch}-unknown-linux-musl.tar.xz" \
       | tar -xJ --strip-components=1 -C /usr/local/bin "watchexec-${WATCHEXEC_VERSION}-${arch}-unknown-linux-musl/watchexec" \
    && useradd --uid 1000 --create-home --shell /usr/sbin/nologin mainloop \
    && mkdir /src \
    && chown 1000:1000 /src
ENV GOCACHE=/home/mainloop/.cache/go-build \
    GOFLAGS=-buildvcs=false
USER 1000:1000
WORKDIR /src
COPY --chown=1000:1000 go.mod go.sum ./
RUN go mod download
COPY --chown=1000:1000 . .
# Warm the build cache so the first `go run` after a restart is fast.
RUN go build -o /dev/null ./cmd/mainloop
CMD ["watchexec", "--restart", "--exts", "go,sql", "--", "go", "run", "./cmd/mainloop", "serve-api"]
