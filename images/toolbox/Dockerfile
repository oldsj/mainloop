# syntax=docker/dockerfile:1
# trunk-ignore-all(trivy/DS-0026): a one-shot pod's script is its process; there is nothing to probe
# checkov:skip=CKV_DOCKER_2: a one-shot pod's script is its process; there is nothing to probe
# The toolbox: the image `command.exec` runs scripts in (docs/specs/privileged-actions.md). It is
# the "VM-like" part of the escape hatch: James's everyday CLIs at pinned versions, with no
# credential baked in. The context is the repository root:
#   docker build -f images/toolbox/Dockerfile .
# Every binary is pinned by version and checked against its release's sha256 in install-tools.
# npm tools install from package-lock.json. Adding a tool is a pull request. There is no docker
# client and no buildah: the toolbox has no daemon or privileges, and crane covers inspecting,
# copying and tagging images. Builds stay in CI.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

# Pods run read-only with an emptyDir at /work, so nothing is written under HOME at build time.
ENV HOME=/work \
    LANG=C.UTF-8 \
    NODE_OPTIONS=--disable-warning=ExperimentalWarning \
    NPM_CONFIG_UPDATE_NOTIFIER=false \
    PATH=/usr/local/lib/toolbox-npm/node_modules/.bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

# ping uses unprivileged ICMP sockets (the pod sets net.ipv4.ping_group_range), so it needs no
# NET_RAW. Its file capability is removed: with every capability dropped, a binary that carries one
# does not start. openssh-client is the client only: LAN hosts stay command.ssh, and there are no keys.
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git gettext-base iputils-ping \
       jq less libatomic1 libcap2-bin openssh-client openssl postgresql-client procps python3 xz-utils dnsutils unzip \
    && setcap -r "$(command -v ping)" \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /usr/local/share/toolbox

COPY images/toolbox/install-tools /usr/local/lib/toolbox/install-tools
RUN /usr/local/lib/toolbox/install-tools \
    && chown -R root:root /usr/local

WORKDIR /usr/local/lib/toolbox-npm
COPY images/toolbox/package.json images/toolbox/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund \
    && node -p "const l=require('./package-lock.json').packages; ['pnpm','wrangler'].map(n=>n+'='+l['node_modules/'+n].version).join('\n')" >>/usr/local/share/toolbox/versions \
    && npm cache clean --force \
    && rm -rf /root/.npm

RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --home-dir /work --no-create-home --shell /bin/bash nonroot \
    && mkdir -p /work \
    && chown 65532:65532 /work

WORKDIR /work
USER 65532:65532
CMD ["bash"]
