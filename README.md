# mainloop

An AI agent orchestrator - one place to manage all AI work, accessible from any device.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) - your attention is the bottleneck, so spawn parallel AI workers and never let your cores idle.

## How It Works

```
You (phone/laptop)
    │
    ▼
┌─────────────────────────────────────────────────────┐
│                   Chat Interface                     │
│           Claude with spawn_task tool               │
│                                                      │
│   "Should I spawn a worker for this? (confirms)"    │
└──────────────┬────────────────┬─────────────────────┘
               │                │
       ┌───────▼──────┐  ┌──────▼───────┐
       │   Worker 1   │  │   Worker 2   │  ...
       │   (Opus)     │  │   (Opus)     │
       │              │  │              │
       │  Feature dev │  │  Bug fix     │
       └──────────────┘  └──────────────┘
```

- **Chat**: Claude responds naturally, uses `spawn_task` tool when you confirm work
- **Workers**: Opus models handle complex tasks in isolated K8s namespaces
- **Durable execution**: Tasks survive restarts via [DBOS](docs/DBOS.md)
- **Conversation memory**: Compaction keeps context across devices/sessions

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
├── frontend/      # SvelteKit + Tailwind v4 (mobile-first responsive)
├── claude-agent/  # Claude Code CLI container
├── models/        # Shared Pydantic models
├── packages/ui/   # Design tokens + theme.css
└── k8s/           # Kubernetes manifests
```

## UI

- **Mobile**: Bottom tab bar (Chat / Tasks / Inbox)
- **Desktop**: Chat with always-visible Tasks sidebar, Inbox overlay
- **Tasks Panel**: Track active workers with live status updates
- **Inbox**: Human review queue for worker questions/approvals

## Agent Workflow

Agents follow a structured workflow: **plan in issue → implement in draft PR → iterate until CI green → ready for human review**.

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Planning  │────►│    Draft    │────►│  Iteration  │────►│   Review    │
│  (GH Issue) │     │    (PR)     │     │  (CI Loop)  │     │   (Human)   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
```

### Phases

1. **Planning (GitHub Issue)** - Agent creates/updates an issue with problem analysis, proposed approach, and implementation plan. The issue is the "thinking out loud" space before code.

2. **Draft PR** - Agent creates a draft PR linked to the issue. Implements in small, logical commits. Uses PR comments to narrate progress and decisions.

3. **Iteration (CI Loop)** - Agent polls GitHub Actions after each push. On failure: analyzes logs, fixes, commits. Continues until green checkmark.

4. **Ready for Review** - Agent marks PR ready and adds summary comment. Human reviewer steps in for final approval.

### Verification

Agents use these tools to verify work before marking ready:
- **LSP server integration** - Real-time type/lint errors
- **`trunk` CLI** - Unified super-linter
- **Project test suites** - Via GitHub Actions

### Project Template (Future)

| Component | Purpose |
|-----------|---------|
| GitHub Actions | CI pipeline (lint, type-check, test) |
| K8s/Helm | Preview environments per PR |
| CNPG operator | Dynamic test databases |
| trunk.yaml | Unified linter config |

## Documentation

- [Architecture](docs/architecture.md) - System design and data flow
- [Development](docs/development.md) - Local setup and commands
- [DBOS Workflows](docs/DBOS.md) - Durable task orchestration
