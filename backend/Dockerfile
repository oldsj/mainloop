# Build stage - use uv image with Python pre-installed
FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS builder

WORKDIR /app

# Enable bytecode compilation for faster startup
ENV UV_COMPILE_BYTECODE=1

# Copy from cache instead of linking
ENV UV_LINK_MODE=copy

# Copy dependency files first (for layer caching)
COPY backend/pyproject.toml backend/uv.lock backend/README.md ./

# Copy shared models package
COPY models ../models

# Install dependencies only (separate from code for layer caching)
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-install-project --no-dev

# Copy source code
COPY backend/src ./src

# Install the project
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev

# Runtime stage
FROM python:3.13-slim

# Install Node.js, git, and Claude CLI (required by claude-agent-sdk and gitpython)
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates git \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code@latest \
    && npm cache clean --force \
    && apt-get purge -y --auto-remove ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /root/.npm

# Create non-root user for Claude and workspace directory
RUN useradd -m -s /bin/bash claude \
    && mkdir -p /home/claude/.claude /workspace \
    && chown -R claude:claude /home/claude /workspace

WORKDIR /app

# Copy the virtual environment from builder (owned by claude)
COPY --from=builder --chown=claude:claude /app/.venv /app/.venv

# Copy source code (owned by claude)
COPY --chown=claude:claude backend/src ./src

# Copy shared models package (owned by claude)
COPY --chown=claude:claude models ../models

# Copy entrypoint script
COPY --chown=claude:claude backend/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Set up PATH
ENV PATH="/app/.venv/bin:$PATH"

# Switch to non-root user (required for Claude SDK bypass permissions)
USER claude

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["uvicorn", "mainloop.api:app", "--host", "0.0.0.0", "--port", "8000"]
