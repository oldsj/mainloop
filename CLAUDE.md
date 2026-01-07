# CLAUDE.md

## Project Overview

**mainloop** is an attention management system - one place to focus, accessible from any device.

- One continuous conversation that persists across devices
- AI workers handle tasks in background
- Unified inbox surfaces what needs attention

**Structure:**

```text
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
make test         # Start services + Playwright UI (keep running)
make test-run     # Run tests headless (separate terminal)
make test-reset   # Clear DB + namespaces between runs
```

**Workflow:**

1. Run `make test` once - starts Kind cluster (backend :8081, frontend :5173)
2. Wait for pods to be ready before running tests
3. Use `make test-run` for quick iterations

**Before running tests**, verify deployments are ready:

```bash
kubectl get pods -n mainloop --context kind-mainloop-test -w
```

Wait for new pods to show `Running` and old pods to terminate.

**Common issues:**

- Port already in use → kill orphan processes or restart `make test`
- Tests fail on old code → wait for deployment rollout to complete
- Flaky tests → use healer agent to fix selectors/timing

**Playwright agents for test maintenance:**

- Don't manually tweak tests - use `playwright-test-healer` to auto-fix failures
- For new features, use `playwright-test-planner` to explore and generate plans
- Use `playwright-test-generator` to create tests from plans

**GitHub mocking:**

Test environment uses `USE_MOCK_GITHUB=true` to avoid hitting the real GitHub API. When adding new GitHub API functions:

1. Add mock implementation to `backend/src/mainloop/services/github_mock.py`
2. Add function name to `funcs_to_mock` list in `backend/src/mainloop/api.py`
3. Mock should return realistic test data, not empty responses

## Key Patterns

- **Pydantic models** in `models/` shared between frontend types and backend
- **Svelte 5 runes**: `$state`, `$derived`, `$effect`, `$props`
- **API calls**: Use `$lib/api.ts`, never hardcode URLs
- **DBOS workflows**: Bump `WORKFLOW_VERSION` in `dbos_config.py` when changing workflow logic
- **HTML**: Be explicit, don't rely on browser defaults (`type="button"`, `rel="noopener"`, etc.)
- **Responsive layouts**: Use `isMobile` store to conditionally render, not CSS hide (avoids duplicate DOM elements)

## Deployment

```bash
make deploy              # Full deploy to k8s
make setup-claude-creds  # Extract Claude credentials from Keychain
```

## Documentation Philosophy

```text
README.md → docs/ → specs/ → tests/
```

Specs define behavior. Tests are the source of truth. Keep docs in sync by running planner agent after feature changes.

## Important

- Delete old code, don't keep for backward compatibility
- `make` runs from repo root
- Don't create markdown docs unless asked
- Don't commit without user review
