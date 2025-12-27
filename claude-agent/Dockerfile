# Use uv with Python pre-installed
FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim

# Install system dependencies including Node.js for Claude CLI
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    git \
    nodejs \
    npm \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Create non-root user
RUN useradd -m -s /bin/bash claude

# Create .claude directory with proper permissions
RUN mkdir -p /home/claude/.claude && chown -R claude:claude /home/claude/.claude

# Set up working directory
WORKDIR /app

# Enable bytecode compilation for faster startup
ENV UV_COMPILE_BYTECODE=1

# Copy from cache instead of linking (for mounted volumes)
ENV UV_LINK_MODE=copy

# Install dependencies using lockfile with BuildKit cache
RUN --mount=type=cache,target=/root/.cache/uv \
    --mount=type=bind,source=uv.lock,target=uv.lock \
    --mount=type=bind,source=pyproject.toml,target=pyproject.toml \
    uv sync --locked --no-install-project --no-dev

# Copy application code
COPY --chown=claude:claude server.py /app/server.py
COPY --chown=claude:claude entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /app/server.py

# Place venv executables at front of path
ENV PATH="/app/.venv/bin:$PATH"

# Reset entrypoint (don't invoke uv)
ENTRYPOINT []

USER claude
WORKDIR /workspace

EXPOSE 8001

CMD ["/entrypoint.sh"]
