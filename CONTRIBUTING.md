# Contributing to Mainloop

Thank you for your interest in contributing to mainloop! This document provides guidelines for contributing to the project.

## Getting Started

1. **Fork the repository** and clone it locally
2. **Set up your environment**:
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   make setup-claude-creds  # Setup Claude Code credentials
   make dev                 # Start local development environment
   ```

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
├── backend/       # Python FastAPI + DBOS workflows
├── frontend/      # SvelteKit + Tailwind v4
├── claude-agent/  # Claude Code CLI container
├── models/        # Shared Pydantic models
├── packages/ui/   # Design tokens + theme
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

# Deployment
make deploy              # Full deploy to k8s
make deploy-loop         # Watch for changes and auto-deploy
```

## Key Patterns

### Backend (Python)

- Use **Pydantic models** from `models/` package for data validation
- Use **DBOS workflows** for durable task execution (see `docs/DBOS.md`)
- Import shared models: `from models import Conversation, Message`

### Frontend (SvelteKit)

- Import theme: `@import '@mainloop/ui/theme.css'` in app.css
- Use stores in `$lib/stores/` for state management
- API calls go through `$lib/api.ts`

### Workflow Versioning (CRITICAL)

When modifying DBOS workflows:

- **Bump `WORKFLOW_VERSION`** in `dbos_config.py` when changing workflow logic
- DBOS replays workflows from checkpoints - changing step order/logic breaks running workflows
- Current version tracked in workflow config

## Pull Request Guidelines

1. **Keep PRs focused**: One feature or fix per PR
2. **Write clear descriptions**: Explain what and why
3. **Update documentation**: If you change behavior, update docs
4. **Test thoroughly**: Ensure local dev environment works
5. **Follow existing patterns**: Match the style of surrounding code

## Questions?

- Check existing issues and discussions
- Read the documentation in `docs/`
- See `CLAUDE.md` for detailed development guide

## License

By contributing, you agree that your contributions will be licensed under the same [Sustainable Use License](LICENSE.md) as the project.
