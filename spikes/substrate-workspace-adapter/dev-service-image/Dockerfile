# Dev-service-gate spike image (gate 4): real Herdr server + a real psql client, driven by the
# same generic exec shim as the preview-gate image, to test a Substrate actor's egress
# connectivity to an external PostgreSQL Service under a narrow CIDR egress policy.
# herdr is copied from the host into the build context by the build script (never committed).
# No credentials are baked in.
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates curl procps postgresql-client \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
COPY herdr /usr/local/bin/herdr
COPY exec-shim.js /usr/local/bin/exec-shim.js
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /work && chown -R agent:agent /work
ENV EXEC_SHIM=/usr/local/bin/exec-shim.js
ENV HOME=/home/agent
ENV HERDR_SESSION=mainloop-dev-service
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
