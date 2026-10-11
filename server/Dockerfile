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
