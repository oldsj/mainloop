# Contributing to Mainloop

Thank you for your interest in contributing to mainloop! This document provides guidelines for contributing to the project.

## Getting Started

1. **Fork the repository** and clone it locally
2. **Set up your environment**:

   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   make dev                 # Start local development environment
   ```

   Authenticate the current Claude integration with `make setup-claude-creds` only when exercising live-agent paths.

## Development Workflow

### Making Changes

1. Create a new branch for your changes:

   ```bash
   git checkout -b feature/your-feature-name
   ```

2. Make your changes following the project conventions (see [Code Style](#code-style) below)

3. Test your changes locally:

   ```bash
   make dev  # Ensure everything works
   ```

4. Commit your changes with clear, descriptive commit messages

5. Push to your fork and create a Pull Request

### Code Style

New backend code goes in `server/` (Go), and new frontend code goes in `web/` (React). `backend/`, `models/`, `frontend/`, and `packages/ui/` are frozen: they take bug fixes and the contract conformance test only, until they are deleted. The API contract will be `api/openapi.yaml`. See [AGENTS.md](AGENTS.md) for the migration rules.

#### Python (Backend)

- Use **Python 3.13+** features and type hints
- Use `uv` for dependency management (`uv add <package>`)
- Never manually edit `pyproject.toml` dependencies
- Follow existing patterns in the codebase
- Use Pydantic models for all data validation
- Import shared models from the `models/` package

#### TypeScript/Svelte (Frontend)

- Use **Svelte 5 runes**: `$state`, `$derived`, `$effect`, `$props`
- Follow mobile-first responsive design patterns
- Use the shared theme from `@mainloop/ui/theme.css`
- Use `$lib/api.ts` for API calls - never hardcode endpoints
- Type everything with TypeScript

#### General

- **Mobile-first**: Design for mobile, enhance for desktop
- **Delete old code**: Don't keep unused code for backward compatibility
- **Avoid over-engineering**: Keep solutions simple and focused
- **Use Makefile targets**: Always run `make` commands from repo root

## Project Structure

```text
mainloop/
├── api/           # OpenAPI contract (in progress)
├── server/        # Go backend for new work (in progress)
├── web/           # React frontend for new work (in progress)
├── backend/       # Frozen Python FastAPI backend
├── frontend/      # Frozen SvelteKit + Tailwind v4
├── models/        # Frozen shared Pydantic models
├── packages/ui/   # Frozen design tokens + theme
└── k8s/           # Kubernetes manifests
```

## Development Commands

```bash
# Start all services with hot reload
make dev

# Backend development
cd backend
uv run mainloop          # Run server
uv add <package>         # Add dependency

# Frontend development
cd frontend
pnpm dev                 # Dev server
pnpm check               # Type check

```

## Key Patterns

### Backend (Python, frozen)

- Use **Pydantic models** from `models/` package for data validation
- Import shared models: `from models import Conversation, Message`

### Frontend (SvelteKit, frozen)

- Import theme: `@import '@mainloop/ui/theme.css'` in app.css
- Use stores in `$lib/stores/` for state management
- API calls go through `$lib/api.ts`

## Running the CI checks in a workspace

Use these commands for the project's CI checks. The capped backend command has
passed on a Linux amd64 development host; gVisor/arm64 workspace qualification of
`make test-backend` is pending.

```bash
# Backend dependency sync (bounded, locked)
make install-backend

# Offline backend and disposable PostgreSQL (60 seconds/test, 9 minutes/suite)
dev-postgres run make test-backend

# Frontend dependency install
pnpm --version
make install-frontend

# Frontend type/diagnostic check
pnpm check

# Frontend unit tests (lib)
make test-frontend

# Lint
make lint
```

After dependency setup, `dev-postgres run make check` runs the complete offline
check set inside `dev-postgres`'s command budget (515 seconds with an image that
predates deadline export), plus up to 30 seconds for cleanup. See the
[foreground deadline table](docs/dev-environment.md#foreground-deadlines) for
per-command caps, network retry limits and timeout diagnostics.

## Pull Request Guidelines

1. **Keep PRs focused**: One feature or fix per PR
2. **Write clear descriptions**: Explain what and why
3. **Update documentation**: If you change behavior, update docs
4. **Test thoroughly**: Ensure local dev environment works
5. **Follow existing patterns**: Match the style of surrounding code

## Questions?

- Check existing issues and discussions
- Read the documentation in `docs/`
- See `AGENTS.md` for repository-wide development guidance

## License

By contributing, you agree that your contributions will be licensed under the same [Sustainable Use License](LICENSE.md) as the project.
