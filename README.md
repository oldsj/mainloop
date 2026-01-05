# mainloop

A single conversation to manage all of your AI sessions - that keeps working while you're away.

Your main conversation thread is the closest digital mapping to your own internal thread of consciousness. Everything else flows into a unified inbox that surfaces only what needs your attention.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) — you are the bottleneck, so spawn parallel AI workers and let them handle the work while you stay in flow.

## How It Works

```text
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

- **Main thread**: One continuous conversation — Claude responds naturally, spawns workers when you confirm
- **Workers**: Opus models handle complex tasks in isolated K8s namespaces
- **Inbox**: Unified attention queue — what needs you surfaces; everything else folds away
- **Persistence**: Conversations and tasks survive restarts via compaction + [DBOS](docs/DBOS.md)

## Quick Start

```bash
# Copy example environment file and configure
cp .env.example .env
# Edit .env with your GitHub username (GHCR_USER) and domains

# Setup Claude credentials (Linux - interactive login)
make setup-claude-creds

# Start all services
make dev

# Frontend: http://localhost:3000
# Backend:  http://localhost:8000/docs
```

## Production Deployment

```bash
# Copy and edit the production config example
cp k8s/apps/mainloop/overlays/prod/personal-config-patch.yaml.example \
   k8s/apps/mainloop/overlays/prod/personal-config-patch.yaml
# Edit with your domains and GitHub username

# Deploy to k8s
kubectl apply -k k8s/apps/mainloop/overlays/prod
```

## Project Structure

```text
mainloop/
├── backend/       # Python FastAPI + DBOS workflows
├── frontend/      # SvelteKit + Tailwind v4 (mobile-first responsive)
├── claude-agent/  # Claude Code CLI container
├── models/        # Shared Pydantic models
├── packages/ui/   # Design tokens + theme.css
└── k8s/           # Kubernetes manifests
```

## UI

- **Mobile**: Bottom tab bar (Chat / Inbox)
- **Desktop**: Chat with always-visible Inbox sidebar

**Inbox** — a unified view of everything that needs your attention:

1. Questions and approvals from workers
2. Active tasks (with expandable live logs)
3. Recent failures (always visible, one-click retry)
4. Recently completed work
5. History (collapsed)

## Agent Workflow

Agents follow a structured workflow: **plan in issue → implement in draft PR → iterate until CI green → ready for human review**.

```text
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

| Component      | Purpose                              |
| -------------- | ------------------------------------ |
| GitHub Actions | CI pipeline (lint, type-check, test) |
| K8s/Helm       | Preview environments per PR          |
| CNPG operator  | Dynamic test databases               |
| trunk.yaml     | Unified linter config                |

## Documentation

- [Architecture](docs/architecture.md) - System design and data flow
- [Development](docs/development.md) - Local setup and commands
- [DBOS Workflows](docs/DBOS.md) - Durable task orchestration
- [Contributing](CONTRIBUTING.md) - How to contribute to mainloop

## License

This project is licensed under the [Sustainable Use License v1.0](LICENSE.md) - a source-available license that allows free use for internal business, non-commercial, and personal purposes.
