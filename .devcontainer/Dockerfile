FROM python:3.13.7-slim-bookworm@sha256:adafcc17694d715c905b4c7bebd96907a1fd5cf183395f0ebc4d3428bd22d92d

ARG TARGETARCH
ENV HOME=/home/nonroot \
    XDG_CACHE_HOME=/home/nonroot/.cache \
    UV_PYTHON_DOWNLOADS=never \
    PATH=/usr/lib/postgresql/16/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

COPY .devcontainer/install-tools /usr/local/lib/mainloop/install-tools
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git make ripgrep \
       build-essential pkg-config libpq-dev xz-utils unzip gnupg locales util-linux \
    && /usr/local/lib/mainloop/install-tools "${TARGETARCH}" \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/nonroot --shell /bin/bash nonroot \
    && mkdir -p /data /workspace /home/nonroot/.cache \
    && chown -R 65532:65532 /data /workspace /home/nonroot

# Only lint configuration enters the public image, never application source or secrets.
COPY .trunk/trunk.yaml /opt/mainloop-lint/.trunk/trunk.yaml
COPY .trunk/configs/ /opt/mainloop-lint/.trunk/configs/
COPY .devcontainer/dev-postgres /usr/local/bin/dev-postgres
WORKDIR /opt/mainloop-lint
RUN git init \
    && trunk install --ci --no-progress \
    && find /home/nonroot/.cache/trunk/tools -type f -exec sha256sum {} + > /opt/mainloop-lint/tool-checksums.txt \
    && chown -R 65532:65532 /home/nonroot /opt/mainloop-lint

WORKDIR /workspace
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s CMD test -w /data && test -w "$HOME" && test -x /usr/local/bin/uv && test -x /usr/lib/postgresql/16/bin/pg_ctl
CMD ["bash"]
