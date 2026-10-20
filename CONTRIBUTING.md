# Contributing to Mainloop

Thank you for your interest in contributing to mainloop! This document provides guidelines for contributing to the project.

## Getting Started

1. **Fork the repository** and clone it locally
2. **Set up your environment**:

   ```bash
   cp .env.example .env     # fill in the values you need
   ```

   The API needs a PostgreSQL 16 server and `AGENT_TOKEN_KEY`. See [Local development](#local-development).

   Native agent paths need a kagent installation and live credentials. Skip them unless your change touches those paths.

## Development Workflow

### Making Changes

1. Create a new branch for your changes:

   ```bash
   git checkout -b feature/your-feature-name
   ```

2. Make your changes following the project conventions (see [Code Style](#code-style) below)

3. Run the checks for the area you changed (see [Verification](#verification))

4. Commit your changes with clear, descriptive commit messages

5. Push to your fork and create a Pull Request

### Code Style

New server code goes in `server/` (Go). New frontend code goes in `web/` (React and TypeScript). The API contract is `api/openapi.yaml`: change it first, then run `go generate ./...` in `server/` so the Go types and routing follow it. See [AGENTS.md](AGENTS.md) for the architecture rules.

#### Go (server)

- Read the environment only in `internal/config`.
- Do not edit `*.gen.go` files. Regenerate them.
- Add dependencies with `go get`, then run `go mod tidy`.
- Keep each process's wiring in `cmd/mainloop`: one binary, one subcommand per process.
- Follow the existing patterns in the package you change.

#### TypeScript/React (web)

- Type everything with TypeScript.
- Use `web/src/api` for API calls. Never hardcode endpoints.
- Use the design tokens in `web/design/` for colour and spacing.
- Use `type="button"` on buttons that do not submit a form.
- Design for mobile first, then enhance for desktop.

#### General

- **Mobile-first**: Design for mobile, enhance for desktop
- **Delete old code**: Don't keep unused code for backward compatibility
- **Avoid over-engineering**: Keep solutions simple and focused
- **Use Make targets from the repo root**: `make lint`, `make fmt` and the `dev-live` targets

## Project Structure

```text
mainloop/
├── api/           # OpenAPI contract (openapi.yaml) and MCP tool manifest
├── server/        # Go server: one binary, one subcommand per process
├── web/           # React app, Vite build, design system
├── k8s/           # Kubernetes bases, overlays and kagent integration
├── docs/          # Specs, architecture and operations notes
└── scripts/       # Development and CI helpers
```

## Local development

Run the API against a disposable PostgreSQL 16 server:

```bash
cd server
export AGENT_TOKEN_KEY=dev-only-key
export DEV_MODE=true                 # accept localhost Hosts and Origins
export DB_HOST=localhost DB_PORT=5432 DB_NAME=postgres DB_USER=mainloop DB_PASSWORD=...
go run ./cmd/mainloop serve-api      # owner API on :8000
curl localhost:8000/health
```

Run the UI with hot reload in a second terminal. It listens on `http://127.0.0.1:5173` and forwards `/api` to `MAINLOOP_API_PROXY` (default `http://127.0.0.1:8000`):

```bash
pnpm install --frozen-lockfile --filter web...
pnpm --dir web dev
```

See [`server/README.md`](server/README.md) for the other subcommands (`serve-mcp`, `serve-git`, `migrate`) and the full environment list.

## Verification

Run the checks for the area you changed: the package(s) you touched in `server/`, the test files
you touched in `web/`. Leave the full suite, `-race`, and the full Vitest run to CI.

```bash
# Repository lint and format (trunk; changed files against origin/main)
make lint
make fmt

# Server (from server/). Set MAINLOOP_TEST_DATABASE_URL to a disposable PostgreSQL 16 server.
# Add -race only when the change touches concurrency.
go test ./internal/<pkg>/...
golangci-lint run ./...
go generate ./... && git status --short   # generated code must not change

# API contract (from the repository root)
pnpm --package=@redocly/cli@2.54.3 dlx redocly lint api/openapi.yaml

# Web (from the repository root, then web/)
pnpm install --frozen-lockfile --filter web...
cd web
node_modules/.bin/tsc -b
node_modules/.bin/eslint .
node_modules/.bin/vitest run <changed test files>
node_modules/.bin/vite build
```

Database tests skip when `MAINLOOP_TEST_DATABASE_URL` is unset.

CI (`.github/workflows/go.yml`) runs the full `go test -race ./...`, sharded into two jobs, on
every PR push and again on merge, using a PostgreSQL 16 service; `go-nightly.yml` reruns the full
suite with `-race -count=1` once a day as a cache backstop.

## Pull Request Guidelines

1. **Keep PRs focused**: One feature or fix per PR
2. **Write clear descriptions**: Explain what and why
3. **Update documentation**: If you change behavior, update docs
4. **Test thoroughly**: Run the checks above for the area you changed
5. **Follow existing patterns**: Match the style of surrounding code

## Questions?

- Check existing issues and discussions
- Read the documentation in `docs/`
- See `AGENTS.md` for repository-wide development guidance

## License

By contributing, you agree that your contributions will be licensed under the same [Sustainable Use License](LICENSE.md) as the project.
