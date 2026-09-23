# syntax=docker/dockerfile:1.7
# Headless Substrate actor image: Substrate owns lifecycle; the shim starts one native CLI
# process per delivered turn. Claude/Codex binaries are supplied in the local build context.
# No credentials are baked in or fetched during startup; the golden actor reaches readiness
# without credentials or external network access.
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends jq ca-certificates git curl procps ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
# The run-specific Substrate MITM CA is a public trust anchor. BuildKit mounts
# it without retaining the input file or putting the PEM in the build command.
ARG EGRESS_MITM_CA_SHA256=""
RUN --mount=type=secret,id=egress-mitm-ca,target=/run/secrets/egress-mitm-ca,required=false \
    if [ -s /run/secrets/egress-mitm-ca ]; then \
      test -n "${EGRESS_MITM_CA_SHA256}" && \
      test "$(sha256sum /run/secrets/egress-mitm-ca | cut -d' ' -f1)" = "${EGRESS_MITM_CA_SHA256}" && \
      install -m 0644 /run/secrets/egress-mitm-ca /usr/local/share/ca-certificates/substrate-egress-mitm.crt && \
      update-ca-certificates; \
    fi
COPY claude /usr/local/bin/claude
COPY codex /usr/local/bin/codex
COPY codex-code-mode-host /usr/local/bin/codex-code-mode-host
COPY bin/start-native-agent /usr/local/bin/start-native-agent
COPY bin/prepare-native-agent-config.cjs /usr/local/bin/prepare-native-agent-config.cjs
COPY agent-config/mainloop-system.txt /etc/agent-config/mainloop-system.txt
COPY exec-shim.js /usr/local/bin/exec-shim.js
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/start-native-agent \
    && mkdir -p /work && chown -R agent:agent /work
ENV EXEC_SHIM=/usr/local/bin/exec-shim.js
ENV NATIVE_AGENT_LAUNCHER=/usr/local/bin/start-native-agent
ENV HOME=/home/agent
ENV WORKSPACE_PATH=/work/repo
ENV CODEX_HOME=/home/agent/.codex
ENV EXEC_SHIM_STATE_DIR=/work/repo/.mainloop/exec-shim
ENV AGENT_SYSTEM_PROMPT_FILE=/etc/agent-config/mainloop-system.txt
ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
