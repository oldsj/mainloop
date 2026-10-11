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

- Migration: new backend work goes in `server/` (Go), and new frontend work goes in `web/` (React). `backend/`, `models/`, `frontend/`, and `packages/ui/` are frozen until they are deleted: bug fixes and the contract conformance test only.
- `api/openapi.yaml` (in progress) is the wire contract. Go and TypeScript types are generated from it, so change the contract first.
- Durable side effects follow the reconciler rule, not a workflow engine: write a durable intent row with an explicit state and an idempotency key, check the remote state before any retry, and never hold a DB lock or transaction across network I/O.
- Mainloop's control plane owns durable product state, policy, message delivery, attention, routing, and audit records.
- Development work runs in isolated workspaces through native Codex, Claude, or qualified local-agent sessions.
- Preserve native session identity, history, tools, compaction, and provider-specific behavior. Do not introduce a proprietary model loop unless the roadmap and task explicitly call for it.
- Use explicit runtime adapters and avoid competing session owners, schedulers, or terminal managers.
- Kubernetes is the intended workspace lifecycle and isolation target. Deploy Kubernetes changes through GitOps; do not directly mutate Argo-managed resources without an explicitly documented operational exception.
- Keep task lifecycle, agent activity, message delivery, user attention, workspace health, and publication state as separate concepts.
- Parallel work is limited by architectural cohesion, not just worker capacity. Resolve shared contracts and helpers before dispatching independent consumers.

Native Claude Code and Codex sessions run as kagent harness agents, from the `oldsj/kagent` fork. New code uses the kagent client in `server/internal/kagent` (in progress); the Python client in `backend/src/mainloop/runtime/` serves only the frozen backend. The Substrate workspace adapter is being removed; do not extend it, and do not add the superseded worker or another workspace runtime.

## Project structure

```text
api/           OpenAPI contract and MCP tool manifest (in progress)
server/        Go backend for new work (in progress)
web/           React frontend for new work (in progress)
backend/       Frozen FastAPI backend, PostgreSQL access, and orchestration
frontend/      Frozen SvelteKit 5 application and Playwright tests
models/        Frozen shared Python/Pydantic models
packages/ui/   Frozen shared frontend theme and UI package
k8s/           Kubernetes bases and overlays
docs/specs/    User-visible behavior specifications
scripts/       Development and test automation
```

## Development conventions

- Preserve unrelated user changes. Inspect `git status` before editing and keep the patch scoped.
- Use `uv run` for project Python commands and `uv add` for Python dependencies. Do not invoke system `python`/`python3` or edit dependency lists manually.
- Use `pnpm` for JavaScript and TypeScript dependencies and scripts.
- Run Make targets from the repository root.
- Validate external data at the boundary: against `api/openapi.yaml` in new code, with Pydantic in the frozen backend.
- New frontend code in `web/` uses React and TypeScript and calls the API only through the generated client, never hardcoded endpoints. Frozen `frontend/` code uses Svelte 5 runes and routes API calls through `frontend/src/lib/api.ts`.
- Use explicit HTML behavior such as `type="button"` and safe external-link attributes.
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

For the shared live instance on the cluster (`make dev-live`, namespace `mainloop-dev`), see [docs/dev-environment.md](docs/dev-environment.md#live-dev-instance).

The local Kubernetes workflow requires DevSpace and Kind. Python packages are managed by `uv`; JavaScript packages are managed by `pnpm`.

DevSpace synchronizes source into the containers. If a Svelte store change appears stale after hot reload, force recompilation of the importing component or clear the Vite cache; a browser refresh alone may retain a stale module reference.

## Verification

Run the smallest checks that establish confidence for the changed area, then expand when risk warrants it.

Common commands:

```bash
make test-backend        # offline backend + scratch PostgreSQL; 60s/test, 9m/suite
make fmt                 # format and check files changed from main
make lint                # lint files changed from main
pnpm check               # workspace frontend/type checks
```

Use `make install-backend install-frontend` for bounded dependency setup and
`dev-postgres run make check` for the complete offline check set inside
`dev-postgres`'s command budget (515 seconds with an image that predates deadline
export), plus up to 30 seconds for cleanup. [Foreground deadline table](docs/dev-environment.md#foreground-deadlines)
lists all caps and timeout diagnostics, including CI jobs.

Set `MAINLOOP_TEST_DATABASE_URL` to a disposable PostgreSQL server first, or use
`dev-postgres run make test-backend` in the development image. The capped command
requires PostgreSQL so database coverage cannot silently skip. Use
`TEST_ARGS='tests.runtime.test_merge_acceptance'` for a focused run and
`MAINLOOP_TEST_TIMINGS=/path/to/timings.json` for module/test timing evidence.
Timeouts print the active test or fixture and all thread stacks, then fail the run.

In a Mainloop workspace, `dev-postgres run make check` is the one command that
installs dependencies and runs all offline checks. Run only the backend suite with
`dev-postgres run make test-backend`; it also installs its dependencies.
The helper starts and stops scratch PostgreSQL and exports `MAINLOOP_TEST_DATABASE_URL`.
Run checks in the foreground and wait for them to finish before ending your turn.

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
