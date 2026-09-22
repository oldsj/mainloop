# Preview-gate spike image: real Herdr server + a real Vite dev server, driven by a generic
# exec shim (see exec-shim.js) standing in for a credentialed native agent's own Bash tool.
# herdr is copied from the host into the build context by the build script (never committed).
# No credentials are baked in.
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates git curl procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
COPY herdr /usr/local/bin/herdr
COPY vite-fixture /work/vite-fixture
COPY exec-shim.js /usr/local/bin/exec-shim.js
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && cd /work/vite-fixture && npm install --no-audit --no-fund \
    && chown -R agent:agent /work
ENV VITE_DIR=/work/vite-fixture
ENV EXEC_SHIM=/usr/local/bin/exec-shim.js
ENV HOME=/home/agent
ENV HERDR_SESSION=mainloop-preview
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
