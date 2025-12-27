# mainloop

An AI agent orchestrator - one place to manage all AI work, accessible from any device.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) - your attention is the bottleneck, so spawn parallel AI workers and never let your cores idle.

## How It Works

```
You (phone/laptop)
    │
    ▼
┌─────────────────────────────────────────────────────┐
│                    Main Thread                       │
│              (Haiku - fast coordinator)              │
│                                                      │
│   Analyzes requests, spawns workers, reports back   │
└──────────────┬────────────────┬─────────────────────┘
               │                │
       ┌───────▼──────┐  ┌──────▼───────┐
       │   Worker 1   │  │   Worker 2   │  ...
       │   (Opus)     │  │   (Opus)     │
       │              │  │              │
       │  Feature dev │  │  Bug fix     │
       └──────────────┘  └──────────────┘
```

- **Main thread**: Fast Haiku model coordinates your requests
- **Workers**: Opus models handle complex tasks in parallel (features, bug fixes, reviews)
- **Durable execution**: Tasks survive restarts via [DBOS](docs/DBOS.md)

## Quick Start

```bash
# Setup credentials (macOS - extracts from Keychain)
make setup-claude-creds

# Start all services
make dev

# Frontend: http://localhost:3000
# Backend:  http://localhost:8000/docs
```

## Project Structure

```
mainloop/
├── backend/       # Python FastAPI + DBOS workflows
├── frontend/      # SvelteKit + Tailwind (mobile-first)
├── claude-agent/  # Claude Code CLI container
├── models/        # Shared Pydantic models
├── packages/ui/   # Design tokens
└── k8s/           # Kubernetes manifests
```

## Documentation

- [Architecture](docs/architecture.md) - System design and data flow
- [Development](docs/development.md) - Local setup and commands
- [DBOS Workflows](docs/DBOS.md) - Durable task orchestration
