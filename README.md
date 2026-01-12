# mainloop

A single conversation to manage all of your AI sessions - that keeps working while you're away.

Your main conversation thread is the closest digital mapping to your own internal thread of consciousness. Everything else flows into a unified inbox that surfaces only what needs your attention.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) — you are the bottleneck, so spawn parallel AI workers and let them handle the work while you stay in flow.

| Desktop                                                                                                                          | Mobile                                                                                                                          |
| -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| <img width="600" alt="mainloop desktop" src="https://github.com/user-attachments/assets/49971afd-c155-4855-a292-5b0c59570066" /> | <img width="150" alt="mainloop mobile" src="https://github.com/user-attachments/assets/3b263abe-9fee-45c5-9ba8-38edd5e5e4a8" /> |

## Conversation Model

Think of it like **Slack**: the main conversation is a channel, and autonomous work happens in threads.

```text
┌─────────────────────────────────────────────────────────────────┐
│  MAIN THREAD (Channel)                                          │
│                                                                 │
│  You: "Add a quickstart to the README"                         │
│                                                                 │
│  Claude: "I'll help with that. What's the repo URL?"           │
│                                                                 │
│  You: "github.com/foo/mainloop"                                │
│                                                                 │
│  Claude: "Got it. Let me explore the codebase..."              │
│          [Planning: explores repo, proposes approach]           │
│          "Here's my plan: ... Ready to implement?"             │
│                                                                 │
│  You: "Looks good, go ahead"                                   │
│                                                                 │
│  Claude: "Starting implementation."                             │
│          ┌─────────────────────────────────────────┐           │
│          │ 🧵 THREAD: Add quickstart to README     │           │
│          │    ⟳ Working · 3 messages              │           │
│          └─────────────────────────────────────────┘           │
│                                                                 │
│  You: "Also, can you explain how DBOS works?"                  │
│                                                                 │
│  Claude: "DBOS is a durable execution framework..."            │
│          [Immediate response - no thread needed]                │
└─────────────────────────────────────────────────────────────────┘
```

### When Work Stays in Main Thread

| Situation           | Example                      | Why                           |
| ------------------- | ---------------------------- | ----------------------------- |
| Questions & answers | "How does X work?"           | Immediate, no autonomous work |
| Clarifying context  | "What repo?" / "Which file?" | Gathering info for a task     |
| Planning discussion | "Here's my approach..."      | Interactive refinement        |
| Quick confirmations | "Should I use TypeScript?"   | Needs your input to proceed   |

### When Work Becomes a Thread

| Situation           | Example                      | Why                        |
| ------------------- | ---------------------------- | -------------------------- |
| Code implementation | Writing files, running tests | Minutes of autonomous work |
| PR creation         | Commits, pushes, CI checks   | Runs in background         |
| Long-running tasks  | Large refactors, migrations  | You shouldn't wait         |

### The Handoff

```text
Main Thread                              Thread
────────────                             ──────
  │
  │  "Add dark mode"
  │       │
  │  "What repo?"
  │       │
  │  "github.com/x/y"
  │       │
  │  [Explores codebase]
  │  [Proposes plan]
  │       │
  │  "Approved"
  │       │
  └───────┼─────────────────────────────► Thread spawns
          │                               │
          │ (you continue chatting)       │ Implements changes
          │                               │ Runs tests
          │                               │ Creates PR
          │                               │
          │◄──────────────────────────────┤ "Done! PR #42 ready"
          │
```

**Key insight**: Planning is synchronous (needs your input), implementation is asynchronous (runs in background).

## Architecture

- **Main thread**: One continuous conversation — Claude responds naturally, spawns threads when you approve
- **Threads**: Opus models handle complex tasks in isolated K8s namespaces
- **Inbox pane**: Shows all active threads — click to expand and interact
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
