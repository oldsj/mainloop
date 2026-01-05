# CLAUDE.md

## Project Overview

**mainloop** is an attention management system - one place to focus, accessible from any device.

- One continuous conversation that persists across devices
- AI workers handle tasks in background
- Unified inbox surfaces what needs attention

**Structure:**

```
mainloop/
├── backend/      # Python 3.13+ FastAPI + PostgreSQL
├── frontend/     # TypeScript/SvelteKit 5 + Tailwind v4
├── claude-agent/ # Claude Code CLI container
├── models/       # Shared Pydantic models
└── k8s/          # Kubernetes manifests
```

## Development

```bash
make dev          # Start all services (frontend :3000, backend :8000)
make fmt          # Format before committing
make lint-all     # Check for issues
```

**Backend:** `uv add <package>` for dependencies (never edit pyproject.toml manually)

**Frontend:** `pnpm add <package>` for dependencies

## Testing

```bash
make test         # Start services + Playwright UI (keep this running)
make test-run     # Run tests headless (in another terminal)
```

**Workflow:**
1. Run `make test` - starts backend (:8081), frontend (:5173), and Playwright UI
2. Keep it running while you develop
3. Use `make test-run` in another terminal for quick headless iterations

Tests in `frontend/tests/` use fixtures from `frontend/tests/fixtures.ts`.

**Mocking:** Set `USE_MOCK_CLAUDE=true` and `USE_MOCK_GITHUB=true` for fast tests without API calls.

## Key Patterns

- **Pydantic models** in `models/` shared between frontend types and backend
- **Svelte 5 runes**: `$state`, `$derived`, `$effect`, `$props`
- **API calls**: Use `$lib/api.ts`, never hardcode URLs
- **DBOS workflows**: Bump `WORKFLOW_VERSION` in `dbos_config.py` when changing workflow logic

## Deployment

```bash
make deploy              # Full deploy to k8s
make setup-claude-creds  # Extract Claude credentials from Keychain
```

## Documentation Philosophy

```
README.md     → High-level overview, features (marketing funnel)
  ↓
docs/         → Feature-specific documentation
  ↓
specs/        → Detailed specifications (links to tests)
  ↓
tests/        → Playwright tests verify specs
```

Specs define desired behavior. Tests are the source of truth.

**Playwright agents automate test creation:**
1. `playwright-test-planner` - browses app, explores UI, generates test plan
2. `playwright-test-generator` - creates tests from plan
3. `playwright-test-healer` - debugs and fixes failing tests

## Important

- Delete old code, don't keep for backward compatibility
- `make` runs from repo root
- Don't create markdown docs unless asked
- Don't commit without user review
