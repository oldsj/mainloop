# Build stage - use uv image with Python pre-installed
FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS builder

WORKDIR /app

# Enable bytecode compilation for faster startup
ENV UV_COMPILE_BYTECODE=1

# Copy from cache instead of linking
ENV UV_LINK_MODE=copy

# Copy dependency files first (for layer caching)
COPY backend/pyproject.toml backend/.python-version backend/uv.lock backend/README.md ./

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

# Runtime stage - minimal image without uv
FROM python:3.13-slim

WORKDIR /app

# Copy the virtual environment from builder
COPY --from=builder /app/.venv /app/.venv

# Copy source code
COPY backend/src ./src

# Copy shared models package
COPY models ../models

# Set up PATH
ENV PATH="/app/.venv/bin:$PATH"

EXPOSE 8000

CMD ["uvicorn", "mainloop.api:app", "--host", "0.0.0.0", "--port", "8000"]
