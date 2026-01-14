# Architecture

## Overview

Mainloop uses a main thread + sessions pattern where your continuous conversation spawns background sessions that appear inline as threaded replies.

```text
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
│  - Chat + inline │            │  - DBOS workflows│
│    sessions      │            │  - Main thread   │
│  - Notifications │            │  - Session queue │
└──────────────────┘            └────────┬─────────┘
                                         │
                    ┌────────────────────┼────────────────────┐
                    │                    │                    │
                    ▼                    ▼                    ▼
           ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
           │   Session 1  │     │   Session 2  │     │   Session N  │
           │   (Claude)   │     │    (Opus)    │     │   (varies)   │
           │              │     │              │     │              │
           │  research    │     │  code work   │     │  any task    │
           └──────────────┘     └──────────────┘     └──────────────┘
```

## Components

### Frontend (SvelteKit)

- Mobile-first responsive UI with chat and sessions tabs
- **Main thread**: Continuous conversation with inline session blocks
- **Inline threading**: Session messages appear as Slack-style notifications in timeline
- **Session views**: Expand inline or zoom to fullscreen conversation
- **Notifications**: Toast alerts when sessions need attention

### Backend (FastAPI + DBOS)

- **Main Thread Workflow**: Per-user conversation that spawns sessions
- **Session Workflows**: Background work with their own conversations
- **PostgreSQL**: Durable workflow state and conversation history
- **SSE**: Real-time session updates and notifications

### Sessions

Sessions are the unified model for all background work:

- **Simple sessions**: Claude conversations without code (research, analysis)
- **Code sessions**: GitHub integration with plan → implement → PR workflow

Each session has:

- Its own conversation (separate from main thread)
- A color for visual distinction in timeline
- Status tracking (active, waiting, completed, etc.)
- Anchor to the main thread message that spawned it

## Data Flow

1. User sends message via frontend
2. Backend adds to main thread conversation
3. Main thread (Claude) decides if session needed
4. If spawning: creates session anchored to user message
5. Session appears inline in main thread timeline
6. Session messages surface as thread notifications
7. User can respond inline or zoom into session
8. Completed sessions post summary back to main thread

## Model Configuration

| Component   | Model          | Purpose                          |
| ----------- | -------------- | -------------------------------- |
| Main thread | Sonnet         | Coordination, spawning decisions |
| Sessions    | Opus (default) | Complex tasks, code generation   |
| Sessions    | Sonnet/Haiku   | Can be specified per-session     |

Sessions can use different models based on task complexity.
