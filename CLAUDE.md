# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mainloop** is an attention management system - a single place to focus, accessible from any device.

The core insight: your main conversation thread is the closest digital mapping to your own internal human thread of consciousness. Everything else—tasks, notifications, reviews—flows into a unified inbox designed to help you manage attention effectively, not fragment it.

**Philosophy:**

- One continuous conversation that persists across all devices
- Parallel AI workers handle tasks in the background
- A unified inbox surfaces only what needs your attention
- Recently completed work and failures bubble up; old stuff folds away
- You stay in flow; the system manages the interrupts

**Monorepo Structure:**

```text
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

| Service     | Hostname                   | Port |
| ----------- | -------------------------- | ---- |
| Frontend    | `mainloop.example.com`     | 3000 |
| Backend API | `mainloop-api.example.com` | 8000 |

Frontend must have `VITE_API_URL=https://mainloop-api.example.com` configured (set via FRONTEND_DOMAIN and API_DOMAIN env vars).

### Data Flow

1. **User** -> Tailscale -> Frontend (SvelteKit)
2. **Frontend** -> Backend API (FastAPI) via configured API domain
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

### Code Quality

Uses [Trunk](https://trunk.io) for linting and formatting. Run from repo root:

```bash
make fmt           # Format and fix staged files
make fmt-all       # Format and fix all files
make lint          # Lint staged files
make lint-all      # Lint all files
```

**Run `make fmt` before committing** to auto-fix formatting issues.

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

### DBOS Durable Workflows

- See docs/DBOS.md for details on how to set up and use DBOS for durable execution.

**Workflow Versioning (CRITICAL):**

- DBOS workflows require determinism - they replay from checkpoints expecting the same step sequence
- Changing workflow step order/logic while workflows are running causes `DBOSUnexpectedStepError`
- **Bump `WORKFLOW_VERSION` in `dbos_config.py`** when changing workflow logic
- DBOS only recovers workflows matching current `application_version` - old versions are ignored
- Current version: `"2"` (interactive plan review in inbox)
- For safe upgrades: use blue-green deployment, let old version drain before retiring

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

```text
frontend/src/lib/
├── components/
│   ├── Chat.svelte          # Main conversation thread
│   ├── TasksPanel.svelte    # Unified inbox (tasks + queue items)
│   ├── TasksBadge.svelte    # Header badge with attention count
│   ├── MobileTabBar.svelte  # Bottom navigation (mobile)
│   └── InputBar.svelte      # Message input
└── stores/
    ├── conversation.ts      # Chat messages state
    ├── tasks.ts             # Worker tasks state + polling
    └── inbox.ts             # Queue items state + polling
```

**Inbox hierarchy** (what needs attention first):

1. Queue items needing response (questions, approvals)
2. Active tasks (in progress with live logs)
3. Recently failed jobs (always visible, with retry)
4. Recently completed (last 30 min)
5. History (collapsed, older stuff)

Layout modes:

- **Desktop (≥768px)**: Chat + always-visible Inbox sidebar
- **Mobile (<768px)**: Bottom tab bar (Chat / Inbox)

### E2E Testing (Playwright)

Tests live in `frontend/tests/` and run against a Kind (Kubernetes in Docker) cluster that tests actual K8s job spawning.

```bash
make test-e2e         # Auto-setup Kind cluster, build/load images, run tests
make test-e2e-ui      # Same setup, interactive UI mode
make test-e2e-debug   # Same setup, debug mode
make kind-delete      # Delete the Kind cluster when done
```

**How it works:**

- `make test-e2e` checks if Kind cluster exists (creates if needed)
- Builds images using Docker cache from `make dev`/`make deploy`
- Loads images into Kind
- Deploys with test Kustomize overlay (simple Postgres StatefulSet)
- Runs Playwright tests against http://localhost:3000
- Cluster persists between runs for fast iteration

**IMPORTANT:**

- Never manually run `kubectl apply -k k8s/apps/mainloop/overlays/test` - always use `make test-e2e` or other make targets
- Test scripts use `--context kind-mainloop-test` flag and won't change your shell's kubectl context
- `make deploy` targets your production cluster based on current kubectl context

**Test structure** (runs sequentially with fail-fast):

- `app.setup.ts` - Page loads, basic elements visible
- `basic/` - Simple conversation flows
- `context/` - Message history and context
- `agents/` - Task/inbox management (desktop)
- `mobile/` - Tab bar navigation (Pixel 5 viewport)

**When modifying UI components**, check if tests need updates:

- Tests use role-based selectors: `getByRole('button', { name: 'Chat' })`
- Some tests check CSS classes for active states (e.g., `toHaveClass(/text-term-accent/)`)
- Mobile tests use `.first()` or `.last()` to handle duplicate elements across viewports
- Run `make test-e2e` after UI changes to catch breakage early

**Key selectors used in tests:**

- Header: `getByRole('heading', { name: '$ mainloop' })`
- Input: `getByPlaceholder('Enter command...')`
- Mobile tabs: `getByRole('button', { name: 'Chat' })`, `getByRole('button', { name: 'Inbox' })`
- Inbox header: `locator('h2:has-text("[INBOX]")')`

### Adding New Features (Docs → Specs → Tests)

When adding a new feature, follow this workflow to keep documentation and tests in sync:

```text
README.md / docs/           →    frontend/specs/*.md    →    frontend/tests/*.spec.ts
(what the feature does)          (test scenarios)            (executable tests)
```

#### Step 1: Document the feature

Update README.md or create a doc in `docs/` describing:

- What the feature does (user-facing behavior)
- Key user flows and interactions
- Edge cases and error states

#### Step 2: Generate test specs

Use the `playwright-test-planner` agent to create test scenarios:

```text
"Use playwright-test-planner to create a test plan for [feature] based on docs/[feature].md"
```

The planner will:

- Read the feature documentation
- Explore the running app at http://localhost:3000
- Generate `frontend/specs/[feature].md` with test scenarios

#### Step 3: Generate tests from specs

Use the `playwright-test-generator` agent:

```text
"Use playwright-test-generator to generate tests from specs/[feature].md"
```

The generator will:

- Execute each step in a real browser
- Record the actions
- Output `frontend/tests/[feature]/*.spec.ts`

#### Step 4: Fix any failures

If tests fail, use the `playwright-test-healer` agent:

```text
"Use playwright-test-healer to fix tests/[feature]/[test].spec.ts"
```

**Spec format** (in `frontend/specs/`):

```markdown
# Feature Test Plan

> See [docs/feature.md](../docs/feature.md) for feature spec

## Test Scenarios

### 1. Scenario Category

#### 1.1 Specific Test Case

**Seed:** tests/fixtures/seed-data.ts

**Steps:**

1. Action to perform
2. Another action

**Expected:**

- What should happen
```

**When modifying existing features**: Update the docs first, then regenerate specs and tests to ensure they stay in sync.

## Important

- Avoid creating markdown documentation unless explicitly asked
- Communicate directly rather than creating temp files
- Delete old code rather than keeping it for backward compatibility
- `make` always runs from repo root
- Always use env-defined API URLs, never hardcode
- Never suggest committing changes before user reviews
