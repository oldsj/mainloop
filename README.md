# mainloop

A central AI agent orchestrator - one place to manage all AI work, accessible from any device.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) - your attention is the bottleneck, so spawn parallel AI processes and never let your cores idle.

## Goals

- **Single Thread** - One continuous conversation across all devices
- **Persistent Memory** - BigQuery stores conversation history and context
- **Mobile Access** - Web UI that works on phone and laptop
- **Claude Code** - Leverage Max subscription via CLI in a container

## Architecture

```
                    ┌─────────────────┐
                    │  Cloudflare     │
                    │  Access + CDN   │
                    └────────┬────────┘
                             │
                    ┌────────▼────────┐
                    │  cloudflared    │
                    │  (K8s pod)      │
                    └────────┬────────┘
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
┌────────▼────────┐ ┌────────▼────────┐ ┌────────▼────────┐
│  Web Frontend   │ │  Backend API    │ │  claude-code    │
│  (SvelteKit)    │ │  (FastAPI +     │ │  MCP Server     │
│                 │ │   MCP Client)   │ │                 │
└─────────────────┘ └────────┬────────┘ └────────┬────────┘
                             │                   │
                             │    MCP Protocol   │
                             ├───────────────────┤
                             │                   │
                    ┌────────▼────────┐          │
                    │    BigQuery     │          │
                    │    (Memory)     │          │
                    └─────────────────┘          │
                                                 │
                                        ┌────────▼────────┐
                                        │  Claude Code    │
                                        │  CLI (Max sub)  │
                                        └─────────────────┘
```

### Communication Flow
1. User sends message from SvelteKit frontend
2. Backend (FastAPI) receives request via Cloudflare Access
3. Backend loads conversation context from BigQuery (recent messages + summaries)
4. Backend sends prompt to claude-agent HTTP API
5. Agent executes Claude Code CLI with `--dangerously-skip-permissions`
6. Claude response flows back: Agent → Backend → Frontend
7. All messages persisted to BigQuery for conversation history

### Memory & Threading

**Conversation Threading** - Core architecture for multi-context work:
- **Main thread**: Daily life, high-level decisions (parent of all threads)
- **Project threads**: Deep work per project → sync daily summaries to main
- **Task threads**: Scoped features → short-lived, archived when done
- **Cross-thread queries**: "What's mainloop status?" pulls from relevant thread
- Each conversation has `parent_id` + `thread_type` + `thread_context`

**Temporal Pyramid** - Memory compression per thread:
- Last 24h: full detail | Last 7d: daily summaries | Last 30d: weekly | Older: monthly
- "Zoom in" to any period for full detail | See [MEMORY_STRATEGY.md](./MEMORY_STRATEGY.md)

## Tech Stack

- **Frontend**: SvelteKit 5 + Tailwind v4 (mobile-first)
- **Backend**: FastAPI + Python 3.13 (uv package manager)
- **AI**: Claude Code CLI via [claude-code-mcp](https://github.com/steipete/claude-code-mcp)
- **Database**: BigQuery (conversation memory)
- **Infrastructure**: Home K8s, Cloudflare Tunnel, ArgoCD
- **Monorepo**: pnpm workspaces + shared models/UI packages

## Development

```bash
# Start all services with hot reload
make dev

# Frontend: http://localhost:3000
# Backend API: http://localhost:8000/docs
# Claude agent: Internal Docker network
```

## Implementation Status

### ✅ Complete
- [x] Monorepo scaffolding (backend, frontend, claude-agent, shared packages)
- [x] SvelteKit chat UI with message bubbles and input
- [x] FastAPI backend with health/chat endpoints
- [x] Docker Compose with hot-reload watches
- [x] Kubernetes manifests for production deployment
- [x] Shared Pydantic models package
- [x] Tailwind v4 design system package

### 🚧 In Progress
- [ ] **Conversation threading** - Core multi-context architecture
  - Add parent_id, thread_type, thread_context to schema
  - Thread creation/switching UI
  - Daily summary sync from child → parent threads
- [ ] **Conversation sidebar** - UI to list/switch between threads
- [ ] **Cross-thread queries** - "What's the status of X?" searches relevant threads

### 🔮 Future
- [ ] **Temporal memory compression** - Pyramid summaries to avoid context bloat (see [MEMORY_STRATEGY.md](./MEMORY_STRATEGY.md))
  - Daily summaries for last 7 days
  - Weekly summaries for last 30 days
  - Monthly summaries for older history
  - Zoom-in capability to load full detail
- [ ] Voice access via phone (Twilio integration)
- [ ] Sub-agent orchestration (spawn K8s jobs for parallel work)
- [ ] Message streaming (Server-Sent Events)
- [ ] Markdown rendering with code syntax highlighting
- [ ] Semantic search across conversation history
