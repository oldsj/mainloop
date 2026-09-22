# Live-agent-gate spike image (gate 5, bounded live proof): real Herdr + real Claude Code /
# Codex CLIs + agentctl, driven by the same generic exec shim as the other gate images.
# herdr/claude/codex/codex-code-mode-host are copied from the host into the build context by the
# build script (never committed). No credentials are baked into this image; they are fetched at
# actor runtime from an in-cluster server reachable only via a narrow EgressPolicy (see
# entrypoint.sh and docs/spikes/substrate-workspace-adapter.md).
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates git curl procps ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
COPY herdr /usr/local/bin/herdr
COPY claude /usr/local/bin/claude
COPY codex /usr/local/bin/codex
COPY codex-code-mode-host /usr/local/bin/codex-code-mode-host
COPY bin/agentctl bin/mainloop /usr/local/bin/
COPY agent-config /etc/agent-config
COPY exec-shim.js /usr/local/bin/exec-shim.js
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/agentctl /usr/local/bin/mainloop \
    && mkdir -p /work && chown -R agent:agent /work
ENV EXEC_SHIM=/usr/local/bin/exec-shim.js
ENV HOME=/home/agent
ENV WORKSPACE_PATH=/work/repo
ENV CODEX_HOME=/home/agent/.codex
ENV AGENT_CONFIG_DIR=/etc/agent-config
ENV HERDR_SESSION=mainloop-live-agent
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
