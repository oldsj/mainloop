# Architecture

## Overview

Mainloop uses a coordinator/worker pattern where a fast main thread agent delegates complex tasks to more capable worker agents.

```
┌─────────────────────────────────────────────────────────────┐
│                      User Devices                           │
│                  (phone, laptop, etc.)                      │
└─────────────────────────┬───────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                   Cloudflare Access                         │
│                (authentication + CDN)                       │
└─────────────────────────┬───────────────────────────────────┘
                          │
          ┌───────────────┴───────────────┐
          ▼                               ▼
┌──────────────────┐            ┌──────────────────┐
│     Frontend     │            │     Backend      │
│    (SvelteKit)   │◄──────────►│    (FastAPI)     │
│                  │            │                  │
│  - Mobile-first  │            │  - DBOS workflows│
│  - Chat UI       │            │  - Main thread   │
│  - Queue view    │            │  - Task queue    │
└──────────────────┘            └────────┬─────────┘
                                         │
                    ┌────────────────────┼────────────────────┐
                    │                    │                    │
                    ▼                    ▼                    ▼
           ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
           │ Claude Agent │     │ Claude Agent │     │ Claude Agent │
           │   Worker 1   │     │   Worker 2   │     │   Worker N   │
           │              │     │              │     │              │
           │  (isolated)  │     │  (isolated)  │     │  (isolated)  │
           └──────────────┘     └──────────────┘     └──────────────┘
```

## Components

### Frontend (SvelteKit)
- Mobile-first responsive UI
- Real-time chat interface
- Human review queue for worker questions/approvals
- Tailwind v4 styling

### Backend (FastAPI + DBOS)
- **Main Thread Workflow**: Per-user coordinator that runs on Haiku (fast, cheap)
- **Worker Workflows**: Task executors that run on Opus (capable, thorough)
- **PostgreSQL**: Durable workflow state and conversation history
- **Task Queue**: DBOS-managed queue for worker distribution

### Claude Agent Container
- Runs Claude Code CLI with Max subscription
- Each worker gets isolated workspace
- Handles git operations, file editing, PR creation

## Data Flow

1. User sends message via frontend
2. Backend receives request (authenticated via Cloudflare Access)
3. Main thread (Haiku) analyzes intent
4. If task needed: spawn worker (Opus) with task details
5. Worker executes autonomously, may ask questions via queue
6. Results flow back to main thread → user

## Model Configuration

| Component | Model | Purpose |
|-----------|-------|---------|
| Main thread | Haiku | Fast coordination, intent analysis |
| Workers | Opus (default) | Complex tasks, code generation |
| Workers | Sonnet/Haiku | Can be overridden per-task |

The main thread can dynamically choose which model a worker uses based on task complexity.
