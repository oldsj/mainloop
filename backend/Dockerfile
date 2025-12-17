# Build stage - install dependencies
FROM python:3.13-slim AS builder

# Copy uv binary from official uv image
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

WORKDIR /app

# Copy backend files for building
COPY backend/pyproject.toml backend/.python-version backend/README.md ./
COPY backend/src ./src

# Copy shared models package
COPY models ../models

# Install dependencies and the project
RUN uv sync --frozen

# Runtime stage
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
