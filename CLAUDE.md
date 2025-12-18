# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mainloop** is an AI agent orchestrator - one place to manage all AI work, accessible from any device. It provides a single continuous conversation across all devices with persistent memory.

**Monorepo Structure:**
```
mainloop/
├── backend/      # Python 3.13+ FastAPI + BigQuery
├── frontend/     # TypeScript/SvelteKit 5 + Tailwind v4 + Cloudflare Pages
├── claude-agent/ # Claude Code CLI container (Max subscription)
├── models/       # Shared Pydantic models (Python)
├── packages/ui/  # Shared design tokens + Tailwind preset
└── k8s/          # Kubernetes manifests
```

## Core Architecture

### Data Flow
1. **User** -> Frontend (SvelteKit) -> Cloudflare Access (auth)
2. **Frontend** -> Backend API (FastAPI) via Cloudflare Tunnel
3. **Backend** -> Claude Agent Container (Claude Code CLI)
4. **Backend** <-> BigQuery (conversation memory)

### Authentication
- Cloudflare Access handles ALL authentication
- No app-level auth needed - trust CF headers
- `Cf-Access-Jwt-Assertion` header contains user identity

## Development Commands

### Initial Setup (macOS)
```bash
# Extract Claude credentials from Keychain and setup .env
make setup-claude-creds

# This will:
# 1. Extract your Claude Code credentials from macOS Keychain
# 2. Create/update .env file with CLAUDE_CREDENTIALS
# 3. Ensure .env is in .gitignore (credentials never committed)
```

**Note**: Run `make setup-claude-creds` again if your Claude credentials expire or you re-authenticate.

### Local Development
```bash
# Start all services with hot reload
make dev
# or
docker compose up --watch

# Frontend: http://localhost:3000
# Backend: http://localhost:8000
# API Docs: http://localhost:8000/docs
```

### Backend (Python + uv)
```bash
cd backend
uv run mainloop          # Run server
make backend-dev         # Run with auto-reload
uv add <package>         # Add dependency (NEVER edit pyproject.toml manually)
```

### Frontend (TypeScript + pnpm)
```bash
cd frontend
pnpm dev                 # Dev server
pnpm build               # Build for production
pnpm check               # Type check
```

### Deployment
```bash
make deploy-frontend     # Deploy to Cloudflare Pages
make deploy              # Full deploy (build images + push + restart K8s)
```

## Key Patterns & Conventions

### Python Development
- **Use `uv`** for dependencies (`uv add/remove <package>`)
- **Pydantic models** for all data validation (in `models/` package)
- **Type hints** throughout (Python 3.13+)
- Import shared models: `from models import Conversation, Message`

### Frontend (SvelteKit + Tailwind)
- **Mobile-first** design - this is primarily a phone interface
- **Use shared UI package**: `import { theme } from '@mainloop/ui/theme'`
- **Tailwind preset**: Import from `@mainloop/ui/tailwind.preset`
- **API calls**: Use `$lib/api.ts` - never hardcode endpoints

### Claude Agent Container
- Claude Code CLI runs with Max subscription
- Credentials loaded from `.env` file (use `make setup-claude-creds` to extract from macOS Keychain)
- Backend communicates with container via internal Docker network
- Workspace mounted at `/workspace` for file operations

### BigQuery (Conversation Memory)
- All conversations persisted to BigQuery
- Schema: conversations, messages tables
- Use parameterized queries for all operations

## Important

- Avoid creating markdown documentation unless explicitly asked
- Communicate directly rather than creating temp files
- Delete old code rather than keeping it for backward compatibility
- `make` always runs from repo root
- Always use env-defined API URLs, never hardcode
- Never suggest committing changes before user reviews
