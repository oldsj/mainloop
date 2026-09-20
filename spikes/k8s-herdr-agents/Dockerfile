# Spike image: real Herdr + real claude/codex CLIs (+ deterministic stand-in agents). Non-root.
# herdr, claude and codex binaries are copied from the host into the build context by the
# build script (never committed). No credentials are baked in: they arrive as Kubernetes Secrets.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates git ripgrep curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
COPY herdr /usr/local/bin/herdr
COPY claude /usr/local/bin/claude
COPY codex /usr/local/bin/codex
COPY codex-code-mode-host /usr/local/bin/codex-code-mode-host
COPY bin/standin-agent /usr/local/bin/standin-agent
# One stand-in implementation installed under two Herdr-recognised kind names.
RUN ln -s standin-agent /usr/local/bin/pi && ln -s standin-agent /usr/local/bin/qwen
COPY bin/agentctl bin/entrypoint.sh bin/mainloop /usr/local/bin/
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
