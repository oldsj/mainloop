# AGENTS.md

## Repository context

Mainloop manages attention, project continuity, and native AI coding-agent sessions from mobile and desktop clients.

## Read before changing code

- `ROADMAP.md` is the whole-project product and architecture direction.
- `docs/specs/` describes current user-visible behavior.
- `docs/architecture.md` and `README.md` describe the existing implementation and may lag the roadmap until the relevant slice updates them.
- `CONTRIBUTING.md` contains public contribution and code-style guidance.
- `CLAUDE.md` only imports this file; edit `AGENTS.md` instead.

When documents differ, do not silently blend future design with current behavior. Use `ROADMAP.md` for intended direction, the task request for current scope, and the specs and code for behavior that exists today. Update affected documentation when a change makes it inaccurate.

## Architectural direction

- The Go server in `server/` owns the API, MCP tools, Git transport, migrations and background work. The React app in `web/` calls the API through the generated client only.
- `api/openapi.yaml` is the wire contract. Go and TypeScript types are generated from it, so change the contract first, then regenerate.
- Durable side effects follow the reconciler rule, not a workflow engine: write a durable intent row with an explicit state and an idempotency key, check the remote state before any retry, and never hold a DB lock or transaction across network I/O.
- Mainloop's control plane owns durable product state, policy, message delivery, attention, routing, and audit records.
- Development work runs in isolated workspaces through native Codex, Claude, or qualified local-agent sessions.
- Preserve native session identity, history, tools, compaction, and provider-specific behavior. Do not introduce a proprietary model loop unless the roadmap and task explicitly call for it.
- Use explicit runtime adapters and avoid competing session owners, schedulers, or terminal managers.
- Kubernetes is the intended workspace lifecycle and isolation target. Deploy Kubernetes changes through GitOps; do not directly mutate Argo-managed resources without an explicitly documented operational exception.
- Native Claude Code and Codex sessions run as kagent harness agents, from the `oldsj/kagent` fork. Go code uses the kagent client in `server/internal/kagent` (in progress). The Substrate workspace adapter is being removed; do not extend it, and do not add the superseded worker or another workspace runtime.
- Keep task lifecycle, agent activity, message delivery, user attention, workspace health, and publication state as separate concepts.
- Parallel work is limited by architectural cohesion, not just worker capacity. Resolve shared contracts and helpers before dispatching independent consumers.

## Project structure

```text
api/           OpenAPI contract (openapi.yaml) and MCP tool manifest (mcp-tools.json)
server/        Go server: one binary, cmd/mainloop, with a subcommand per process
web/           React app (Vite, TanStack Router, Tailwind); design system in web/design/
k8s/           Kubernetes bases, overlays and kagent integration (apps/mainloop, integrations/kagent)
docs/specs/    User-visible behavior specifications
scripts/       Development and CI helper scripts
```

## Development conventions

- Preserve unrelated user changes. Inspect `git status` before editing and keep the patch scoped.
- Go: run commands from `server/`. Add dependencies with `go get`, then run `go mod tidy`. Do not edit `*.gen.go` files; run `go generate ./...` instead.
- JavaScript: use `pnpm` to install and run `web/` scripts. CI calls tools from `web/node_modules/.bin` directly.
- Validate external data at the boundary against `api/openapi.yaml`.
- New frontend code in `web/` uses React and TypeScript and calls the API only through the generated client in `web/src/api`, never hardcoded endpoints.
- Use explicit HTML behavior such as `type="button"` and safe external-link attributes.
- Run Make targets from the repository root.
- Prefer deleting obsolete code over keeping unused compatibility paths.

## Common development commands

```bash
# API: needs a PostgreSQL 16 server and AGENT_TOKEN_KEY (see server/README.md)
cd server && go run ./cmd/mainloop serve-api          # :8000

# UI with hot reload on http://127.0.0.1:5173; forwards /api to MAINLOOP_API_PROXY (default 127.0.0.1:8000)
pnpm --dir web dev

# Shared live instance in namespace mainloop-dev (see docs/dev-environment.md)
make dev-live             # deploy, then sync server/ and web/ and stream logs
make dev-live-status      # pods, database, route and dev agents
make dev-live-purge       # delete the namespace and the *-dev agents (typed confirmation)
```

## Verification

Run the smallest checks that establish confidence for the changed area, then expand when risk warrants it. In a task workspace, leave the full suite, `-race`, and the full Vitest run to CI: run only the packages and test files you changed.

```bash
# Repository lint and format (trunk; changed files against origin/main)
make lint
make fmt                  # applies fixes, then rechecks
make lint-all             # whole repository; slow

# Server (run from server/). For the package(s) you changed; add -race only when the
# change touches concurrency. Database tests need MAINLOOP_TEST_DATABASE_URL, a disposable
# PostgreSQL 16 server; they skip without it.
go test ./internal/<pkg>/...
golangci-lint run ./...
go generate ./... && git status --short    # generated code must not change

# API contract (run from the repository root)
pnpm --package=@redocly/cli@2.54.3 dlx redocly lint api/openapi.yaml

# Web (run from the repository root, then web/)
pnpm install --frozen-lockfile --filter web...
cd web
node_modules/.bin/tsc -b
node_modules/.bin/eslint .
node_modules/.bin/vitest run <changed test files>
node_modules/.bin/vite build
pnpm build:design && pnpm check:design    # CI also installs Chromium first
```

`web/` has a Playwright config and an `e2e` script, but no CI job runs it. Run browser tests only when a task asks for them, and only against a known target.

CI (`.github/workflows/go.yml`) runs the checks above at full breadth on every PR push and again on
merge: lint, the full `go test -race ./...` (sharded into two jobs so neither runs close to its
timeout), `go generate`/`go mod tidy` drift, the MCP contract check, and the prod image build.
`go-nightly.yml` reruns the full suite with `-race -count=1` once a day as a backstop against a
stale cached result between PRs.

## Building in a Mainloop workspace

Generic workspace, cache and publishing guidance comes from Mainloop's agent instructions; these are specific to this repository.

- This repository's environment can follow the `cache-main` workspace image (see [Workspace cache image](.github/README.md#workspace-cache-image) and `images/workspace-cache`). It carries the Go build cache, Go modules and pnpm store for one `main` commit under `/opt/cache`, built from a clone at `/data/workspace`. A clean checkout of that commit compiles nothing. The image is rebuilt when dependencies or the image's inputs change, and daily otherwise, so it can be up to a day behind `main`.
- Go stamps the revision and dirty state into main packages (`cmd/mainloop`, `tools/genfallback`, `tools/mcpcontract`). Once HEAD moves past the cached commit, or the tree has uncommitted changes, those three rebuild; everything else still hits the cache. `go build -buildvcs=false ./...` skips the stamp for checks; do not use it for release builds.
- With that image, the pnpm virtual store sits outside the project, so `web/node_modules` is links. Do not copy or move it.
- The caches do not speed up `tsc -b`, `vitest run` or `vite build`. The full Vitest run took about five minutes in one measured workspace, so run the test files you changed first.

## Live and cluster work

- Default automated tests for native-agent adapters must use sanitized fixtures or fakes. Keep live proofs opt-in and bounded.
- Do not run live-agent tests, `mainloop smoke --live`, subscription-consuming commands, deployments, destructive resets (`mainloop migrate reset`), or production commands unless the task explicitly requires them and their target is known.
- For Kubernetes commands, always specify the intended context. Tests must not rely on a developer's current context or mutate production resources.
- Task agents try changes on mainloop-dev through the `dev_*` MCP tools (see `docs/specs/devloop.md`); `make dev-live` needs a kube context that a task workspace does not have, so it is for operators only.

## Documentation

- Keep tracked documentation portable and free of machine-specific assumptions.
- Record durable architectural intent in `ROADMAP.md`. Keep task-specific plans in their issue, pull request, or task context unless they are intended as lasting project documentation.
- User-visible behavior changes require corresponding updates under `docs/specs/` and tests where practical.
- Clearly label proposed, measured, and implemented behavior. Do not present a fixture, mocked integration, or agent claim as live proof.
- Commit only sanitized fixtures needed for repeatable tests, not raw session logs.

## Reliability

- Reconcile native-session ownership and delivery state before retrying an uncertain action. Do not blindly replay prompts or start a second writer.
- Never weaken tests, security boundaries, or evidence requirements merely to make a check pass. Document unsupported capabilities and gaps directly.
