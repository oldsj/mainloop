# Use uv with Python pre-installed
FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim

# Install system dependencies including Node.js for Claude CLI and GitHub CLI
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl=7.88.1-10+deb12u5 \
    git=1:2.39.2-1.1 \
    nodejs=18.19.0+dfsg-6~deb12u1 \
    npm=9.6.7~ds1-1 \
    gnupg=2.2.40-1.1 \
    && rm -rf /var/lib/apt/lists/*

# Install GitHub CLI (gh)
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh=2.42.0 \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI and create non-root user
RUN npm install -g @anthropic-ai/claude-code@latest \
    && useradd -m -s /bin/bash claude \
    && mkdir -p /home/claude/.claude \
    && chown -R claude:claude /home/claude/.claude

# Set up working directory
WORKDIR /app

# Enable bytecode compilation for faster startup
ENV UV_COMPILE_BYTECODE=1

# Disable Python output buffering for real-time logs
ENV PYTHONUNBUFFERED=1

# Copy from cache instead of linking (for mounted volumes)
ENV UV_LINK_MODE=copy

# Install dependencies using lockfile with BuildKit cache
RUN --mount=type=cache,target=/root/.cache/uv \
    --mount=type=bind,source=uv.lock,target=uv.lock \
    --mount=type=bind,source=pyproject.toml,target=pyproject.toml \
    uv sync --locked --no-install-project --no-dev

# Copy application code
COPY --chown=claude:claude server.py /app/server.py
COPY --chown=claude:claude job_runner.py /app/job_runner.py
COPY --chown=claude:claude entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /app/server.py /app/job_runner.py

# Place venv executables at front of path
ENV PATH="/app/.venv/bin:$PATH"

# Reset entrypoint (don't invoke uv)
ENTRYPOINT []

USER claude
WORKDIR /workspace

EXPOSE 8001

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8001/health || exit 1

CMD ["/entrypoint.sh"]
