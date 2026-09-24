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

# Development stage - includes uv for live dependency updates
FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS dev

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/*

RUN useradd -m -s /bin/bash mainloop

WORKDIR /app

# Copy dependency files (owned by claude for live updates)
COPY --from=builder --chown=mainloop:mainloop /app/.venv /app/.venv
COPY --chown=mainloop:mainloop backend/pyproject.toml backend/uv.lock backend/README.md ./

# Copy source code (owned by claude)
COPY --chown=mainloop:mainloop backend/src ./src

# Copy shared models package (owned by claude)
COPY --chown=mainloop:mainloop models ../models

# Set up PATH for venv and uv
ENV PATH="/app/.venv/bin:$PATH"

# Switch to non-root user
USER mainloop

EXPOSE 8000

CMD ["uvicorn", "mainloop.api:app", "--host", "0.0.0.0", "--port", "8000", "--reload", "--reload-dir", "/app/src"]

# Runtime stage (production)
FROM python:3.13-slim AS prod

# Install the health-check client and CA certificates.
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && apt-get purge -y --auto-remove ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/*

RUN useradd -m -s /bin/bash mainloop

WORKDIR /app

# Copy the virtual environment from builder (owned by claude)
COPY --from=builder --chown=mainloop:mainloop /app/.venv /app/.venv

# Copy source code (owned by claude)
COPY --chown=mainloop:mainloop backend/src ./src

# Copy shared models package (owned by claude)
COPY --chown=mainloop:mainloop models ../models

# Set up PATH
ENV PATH="/app/.venv/bin:$PATH"

# Run without root privileges.
USER mainloop

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "mainloop.api:app", "--host", "0.0.0.0", "--port", "8000"]
