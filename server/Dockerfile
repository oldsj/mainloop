# trunk-ignore-all(trivy/DS-0026): Kubernetes probes /health; the image has no HTTP client for a HEALTHCHECK
# checkov:skip=CKV_DOCKER_2: Kubernetes probes /health; the image has no HTTP client to run a HEALTHCHECK
# Build context is this directory: docker build --target prod server
ARG GO_VERSION=1.27.2

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mainloop ./cmd/mainloop

# prod: git for the git transport, util-linux for the workspace helpers, CA certificates for
# TLS to GitHub and kagent.
FROM debian:bookworm-slim AS prod
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 1000 --create-home --shell /usr/sbin/nologin mainloop
COPY --from=build /out/mainloop /usr/local/bin/mainloop
USER 1000:1000
EXPOSE 8000 8002 8003 8004
ENTRYPOINT ["/usr/local/bin/mainloop"]
CMD ["serve-api"]

# dev: Go toolchain with watchexec to rebuild on change; the source is synced in at /src.
FROM golang:${GO_VERSION}-bookworm AS dev
ARG TARGETARCH
ARG WATCHEXEC_VERSION=2.8.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends git util-linux xz-utils \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && arch="$(case "${TARGETARCH:-amd64}" in arm64) echo aarch64;; *) echo x86_64;; esac)" \
    && curl -fsSL "https://github.com/watchexec/watchexec/releases/download/v${WATCHEXEC_VERSION}/watchexec-${WATCHEXEC_VERSION}-${arch}-unknown-linux-musl.tar.xz" \
       | tar -xJ --strip-components=1 -C /usr/local/bin "watchexec-${WATCHEXEC_VERSION}-${arch}-unknown-linux-musl/watchexec"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
CMD ["watchexec", "--restart", "--exts", "go", "--", "go", "run", "./cmd/mainloop", "serve-api"]
