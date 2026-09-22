# AGENTS.md

## Repository context

Mainloop manages attention, project continuity, and native AI coding-agent sessions from mobile and desktop clients.

## Read before changing code

- `ROADMAP.md` is the whole-project product and architecture direction.
- `docs/specs/` describes current user-visible behavior.
- `docs/architecture.md` and `README.md` describe the existing implementation and may lag the roadmap until the relevant slice updates them.
- `CONTRIBUTING.md` contains public contribution and code-style guidance.

When documents differ, do not silently blend future design with current behavior. Use `ROADMAP.md` for intended direction, the task request for current scope, and the specs and code for behavior that exists today. Update affected documentation when a change makes it inaccurate.

## Architectural direction

- Reuse the SvelteKit frontend, FastAPI backend, PostgreSQL, shared Pydantic models, and suitable DBOS durability mechanisms.
- Mainloop's control plane owns durable product state, policy, message delivery, attention, routing, and audit records.
- Development work runs in isolated workspaces through native Codex, Claude, or qualified local-agent sessions.
- Preserve native session identity, history, tools, compaction, and provider-specific behavior. Do not introduce a proprietary model loop unless the roadmap and task explicitly call for it.
- Use explicit runtime adapters and avoid competing session owners, schedulers, or terminal managers.
- Kubernetes is the intended workspace lifecycle and isolation target. Deploy Kubernetes changes through GitOps; do not directly mutate Argo-managed resources without an explicitly documented operational exception.
- Keep task lifecycle, agent activity, message delivery, user attention, workspace health, and publication state as separate concepts.
- Parallel work is limited by architectural cohesion, not just worker capacity. Resolve shared contracts and helpers before dispatching independent consumers.

The `claude-agent/` service and Claude Agent SDK paths are part of the existing implementation. The roadmap intends to replace them deliberately with native-agent integration; do not extend them as the new long-term architecture unless a compatibility change requires it.

## Project structure

```text
backend/       FastAPI, DBOS workflows, PostgreSQL access, and orchestration
frontend/      SvelteKit 5 application and Playwright tests
models/        Shared Python/Pydantic models
packages/ui/   Shared frontend theme and UI package
claude-agent/  Existing Claude Agent SDK worker implementation
k8s/           Kubernetes bases and overlays
docs/specs/    User-visible behavior specifications
scripts/       Development and test automation
```

## Development conventions

- Preserve unrelated user changes. Inspect `git status` before editing and keep the patch scoped.
- Use `uv run` for project Python commands and `uv add` for Python dependencies. Do not invoke system `python`/`python3` or edit dependency lists manually.
- Use `pnpm` for JavaScript and TypeScript dependencies and scripts.
- Run Make targets from the repository root.
- Import shared backend models from `models/`; validate external data with Pydantic.
- Use Svelte 5 runes and TypeScript. Route frontend API calls through `frontend/src/lib/api.ts` rather than hardcoding endpoints.
- Use explicit HTML behavior such as `type="button"` and safe external-link attributes.
- Bump `WORKFLOW_VERSION` in `backend/src/mainloop/workflows/dbos_config.py` when a DBOS workflow change would alter replayed behavior.
- Prefer deleting obsolete code when a migration is complete rather than retaining unused compatibility paths.

Common development commands, run from the repository root:

```bash
make dev          # DevSpace + Kind with hot reload
make dev-stop     # stop the development environment
make dev-reset    # reset local development data
make dev-clear-cache
scripts/dev-ui.sh up     # this worktree's frontend (Vite, hot reload) against the shared Kind backend
```

`scripts/dev-ui.sh` is safe with several worktrees running at once: each gets its own Vite port and they share one backend port-forward. The Kind backend itself is shared, so a backend image loaded from one worktree serves all of them.

The local Kubernetes workflow requires DevSpace and Kind. Python packages are managed by `uv`; JavaScript packages are managed by `pnpm`.

DevSpace synchronizes source into the containers. If a Svelte store change appears stale after hot reload, force recompilation of the importing component or clear the Vite cache; a browser refresh alone may retain a stale module reference.

## Verification

Run the smallest checks that establish confidence for the changed area, then expand when risk warrants it.

Common commands:

```bash
make fmt                 # format and check files changed from main
make lint                # lint files changed from main
pnpm check               # workspace frontend/type checks
```

The Playwright suites (`fast`, `mobile`, `e2e`) and the live worker e2e script are disabled: CI no longer runs them, and `make test`, `make test-run`, `make test-ci`, `make test-worker-e2e`, and the frontend `pnpm test` scripts refuse to start unless `ENABLE_E2E=1` is set. The specs and test files are kept for reference and for a deliberate opt-in run.

Some historical tests and Make targets invoke live agents, external services, containers, or Kubernetes. Do not run the browser `e2e` tests, live-agent tests, subscription-consuming commands, deployments, destructive resets, or production commands unless the task explicitly requires them and their target is known. Default automated tests for new native-agent adapters must use sanitized fixtures or fakes; keep live proofs opt-in and bounded.

For Kubernetes commands, always specify the intended context. Tests must not rely on a developer's current context or mutate production resources.

## Documentation

- Keep tracked documentation portable and free of machine-specific assumptions.
- Record durable architectural intent in `ROADMAP.md`. Keep task-specific plans in their issue, pull request, or task context unless they are intended as lasting project documentation.
- User-visible behavior changes require corresponding updates under `docs/specs/` and tests where practical.
- Clearly label proposed, measured, and implemented behavior. Do not present a fixture, mocked integration, or agent claim as live proof.
- Commit only sanitized fixtures needed for repeatable tests, not raw session logs.

## Reliability

- Reconcile native-session ownership and delivery state before retrying an uncertain action. Do not blindly replay prompts or start a second writer.
- Never weaken tests, security boundaries, or evidence requirements merely to make a check pass. Document unsupported capabilities and gaps directly.
