# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mainloop** is an AI agent orchestrator - one place to manage all AI work, accessible from any device. It provides a single continuous conversation across all devices with persistent memory.

**Monorepo Structure:**
```
mainloop/
├── backend/      # Python 3.13+ FastAPI + PostgreSQL
├── frontend/     # TypeScript/SvelteKit 5 + Tailwind v4
├── claude-agent/ # Claude Code CLI container (Max subscription)
├── models/       # Shared Pydantic models (Python)
├── packages/ui/  # Shared design tokens + Tailwind preset
└── k8s/          # Kubernetes manifests (Tailscale Gateway routing)
```

## Core Architecture

### Production Networking (Tailscale Gateway)
All production traffic routes through Tailscale Gateway - no public ingress. HTTPRoutes defined in `k8s/apps/mainloop/base/httproute.yaml`:

| Service | Hostname | Port |
|---------|----------|------|
| Frontend | `mainloop.olds.network` | 3000 |
| Backend API | `mainloop-api.olds.network` | 8000 |

Frontend must have `VITE_API_URL=https://mainloop-api.olds.network` configured.

### Data Flow
1. **User** -> Tailscale -> Frontend (SvelteKit)
2. **Frontend** -> Backend API (FastAPI) via `mainloop-api.olds.network`
3. **Backend** -> Claude with `spawn_task` MCP tool
4. **Claude** -> Spawns K8s worker jobs when user confirms
5. **Backend** <-> PostgreSQL (conversation memory + compaction)

### Authentication
- Tailscale handles network-level access control
- Only devices on the tailnet can reach the services

### Conversation Continuity
Conversations persist across devices and sessions via **compaction**:
- `summary`: Compacted summary of older messages (stored in PostgreSQL)
- `summarized_through_id`: Last message included in summary
- `message_count`: Triggers compaction at threshold
- Recent unsummarized messages fetched for context

This replaces Claude session resumption which didn't survive pod restarts.

### spawn_task Tool
Claude has an MCP tool to spawn workers. It:
1. Asks user for confirmation before spawning
2. Suggests recent repos from user's history
3. Validates GitHub URL format
4. Creates WorkerTask and enqueues via DBOS
5. Tracks repo in user's MRU list

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
# Push Claude credentials to 1Password vault (for k8s)
make setup-claude-creds-k8s

# Deploy services
make deploy-frontend     # Build and deploy frontend to k8s
make deploy              # Full deploy (build images + push + restart K8s)
make deploy-loop         # Watch for changes and auto-deploy (requires watchexec)
```

**Note**: Run `make setup-claude-creds-k8s` when credentials change to update the 1Password vault. The k8s deployment will automatically use the credentials from the vault.

## Key Patterns & Conventions

### Python Development
- **Use `uv`** for dependencies (`uv add/remove <package>`)
- **Pydantic models** for all data validation (in `models/` package)
- **Type hints** throughout (Python 3.13+)
- Import shared models: `from models import Conversation, Message`

### Frontend (SvelteKit 5 + Tailwind v4)
- **Mobile-first** responsive design with desktop enhancements
- **Svelte 5 runes**: Use `$state`, `$derived`, `$effect`, `$props`
- **Shared theme**: `@import '@mainloop/ui/theme.css'` in app.css
- **Stores**: Use `$lib/stores/` for state (conversation, tasks, inbox)
- **API calls**: Use `$lib/api.ts` - never hardcode endpoints

### Claude Agent Container
- Claude Code CLI runs with Max subscription
- Credentials loaded from `.env` file (use `make setup-claude-creds` to extract from macOS Keychain)
- Backend communicates with container via internal Docker network
- Workspace mounted at `/workspace` for file operations

### DBOS
- See docs/DBOS.md for details on how to set up and use DBOS for durable execution.

### Worker Workflow (CI Verification Loop)
Workers iterate until GitHub Actions pass before requesting human review:
1. Create draft PR with implementation
2. Poll GitHub Actions check status
3. On failure: analyze logs, spawn fix job, push fix
4. Retry up to 5 times before failing
5. Only proceed to human review when CI is green

Key files:
- `backend/src/mainloop/workflows/worker.py` - Main workflow with CI loop
- `backend/src/mainloop/services/github_pr.py` - `get_check_status()`, `get_check_failure_logs()`
- `claude-agent/job_runner.py` - `fix` mode for CI failures

### Frontend Components
```
frontend/src/lib/
├── components/
│   ├── Chat.svelte          # Main chat interface
│   ├── TasksPanel.svelte    # Active workers list
│   ├── TasksBadge.svelte    # Header badge with count
│   ├── InboxPanel.svelte    # Human review queue
│   ├── InboxBadge.svelte    # Header badge with unread count
│   ├── MobileTabBar.svelte  # Bottom navigation (mobile)
│   └── InputBar.svelte      # Message input
└── stores/
    ├── conversation.ts      # Chat messages state
    ├── tasks.ts             # Active tasks state + polling
    └── inbox.ts             # Inbox items state + polling
```

Layout modes:
- **Desktop (≥768px)**: Header with badges, Tasks sidebar always visible, Inbox overlay
- **Mobile (<768px)**: Bottom tab bar, full-screen panels per tab

## Important

- Avoid creating markdown documentation unless explicitly asked
- Communicate directly rather than creating temp files
- Delete old code rather than keeping it for backward compatibility
- `make` always runs from repo root
- Always use env-defined API URLs, never hardcode
- Never suggest committing changes before user reviews
