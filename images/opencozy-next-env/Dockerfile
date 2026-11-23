# syntax=docker/dockerfile:1
# Development environment for oldsj/opencozy-next workspaces on Mainloop (the onboarding pilot).
# It lives here, not in opencozy-next: the image is Mainloop's workspace infrastructure.
#
# Versions match opencozy-next's main: Go from go.mod, Node from .node-version, pnpm from
# web/package.json's packageManager, Postgres from compose.yaml. Postgres runs inside the
# workspace (ocn-postgres, the project's workspace service); there is no Docker in a workspace.
# agent-browser drives Debian's Chromium for screenshots; Playwright's browsers are not included,
# so e2e runs in the repository's CI.
#
# Registration requires USER 65532:65532 and a digest reference (docs/specs/environments.md).
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

# Workspace runtimes move HOME and XDG_CACHE_HOME onto /data, so nothing is baked under HOME.
ENV HOME=/home/nonroot \
    GOTOOLCHAIN=local \
    AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium \
    PATH=/usr/local/go/bin:/usr/lib/postgresql/18/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

COPY images/opencozy-next-env/install-tools /usr/local/lib/opencozy-next-env/install-tools
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git make \
       build-essential pkg-config xz-utils unzip gnupg locales chromium fonts-liberation \
    && /usr/local/lib/opencozy-next-env/install-tools \
    && rm -rf /var/lib/apt/lists/*

COPY images/opencozy-next-env/ocn-postgres /usr/local/bin/ocn-postgres

RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/nonroot --shell /bin/bash nonroot \
    && mkdir -p /data /workspace \
    && chown -R 65532:65532 /data /workspace /home/nonroot

WORKDIR /workspace
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s CMD test -w /data && test -x /usr/lib/postgresql/18/bin/postgres
CMD ["bash"]
