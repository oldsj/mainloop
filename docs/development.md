# Development

## Prerequisites

- Docker & Docker Compose
- Node.js 20+ with pnpm
- Python 3.13+ with uv
- Claude Code CLI authenticated (Max subscription)

## Initial Setup

```bash
# Extract Claude credentials from macOS Keychain
make setup-claude-creds

# This creates .env with CLAUDE_CREDENTIALS
# Run again if credentials expire
```

## Running Locally

```bash
# Start all services with hot reload
make dev

# Or directly with docker compose
docker compose up --watch
```

| Service | URL |
|---------|-----|
| Frontend | http://localhost:3000 |
| Backend API | http://localhost:8000 |
| API Docs | http://localhost:8000/docs |

## Backend Development

```bash
cd backend

# Run server
uv run mainloop

# Add dependency (never edit pyproject.toml manually)
uv add <package>

# Remove dependency
uv remove <package>
```

## Frontend Development

```bash
cd frontend

# Dev server
pnpm dev

# Type check
pnpm check

# Build
pnpm build
```

## Project Layout

```
mainloop/
├── backend/           # FastAPI + DBOS
│   └── src/mainloop/
│       ├── workflows/ # DBOS durable workflows
│       ├── services/  # Claude agent client
│       └── db/        # Database operations
├── frontend/          # SvelteKit
│   └── src/
│       ├── routes/    # Pages
│       └── lib/       # Shared code
├── claude-agent/      # Claude Code container
├── models/            # Shared Pydantic models
├── packages/ui/       # Design tokens
├── k8s/               # Kubernetes manifests
└── docs/              # Documentation
```

## Deployment

```bash
# Push credentials to 1Password (for k8s)
make setup-claude-creds-k8s

# Deploy frontend to Cloudflare Pages
make deploy-frontend

# Full deploy (images + k8s)
make deploy
```
