.PHONY: help dev dev-stop dev-reset dev-logs dev-shell dev-legacy install clean lint lint-all fmt fmt-all build-backend build-frontend build-all push-backend push-frontend push-all build-all-parallel push-all-parallel kind-create kind-delete kind-load kind-secrets kind-deploy kind-reset kind-logs kind-shell test test-backend test-run test-reset test-ci debug-tasks debug-task debug-retry debug-logs debug-db

# Load .env file if it exists
-include .env
export

# Configuration
# Set GHCR_USER in .env file (see .env.example)
GHCR_REGISTRY := ghcr.io
GHCR_USER ?= yourusername
IMAGE_TAG ?= latest
BACKEND_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-backend:$(IMAGE_TAG)
FRONTEND_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-frontend:$(IMAGE_TAG)

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# =============================================================================
# Development (DevSpace + Kind) - Hot reload via file sync
# =============================================================================

dev: ## Start dev environment with hot reload (DevSpace + Kind)
	devspace dev --kube-context kind-$(KIND_CLUSTER_NAME) -n mainloop

dev-stop: ## Stop DevSpace and purge resources
	devspace purge --kube-context kind-$(KIND_CLUSTER_NAME) -n mainloop

dev-reset: ## Reset local database and restart the backend
	@./scripts/kind/reset-data.sh

dev-clear-cache: ## Clear Vite cache (fixes stale HMR issues)
	kubectl --context kind-$(KIND_CLUSTER_NAME) -n mainloop exec deployment/mainloop-frontend-devspace -- rm -rf /app/node_modules/.vite 2>/dev/null || true
	@echo "Vite cache cleared. Refresh browser."

dev-logs: ## Tail backend logs
	devspace logs -f --kube-context kind-$(KIND_CLUSTER_NAME) -n mainloop

dev-shell: ## Open shell in backend pod
	devspace enter --kube-context kind-$(KIND_CLUSTER_NAME) -n mainloop

dev-legacy: ## Start with docker compose (no hot reload)
	docker compose up --build --watch

# =============================================================================
# Dependencies
# =============================================================================

install: ## Install all dependencies
	pnpm install
	cd backend && uv sync
	cd models && uv sync

clean: ## Clean build artifacts
	rm -rf frontend/.svelte-kit frontend/build
	rm -rf backend/.venv models/.venv
	find . -type d -name "__pycache__" -exec rm -rf {} +

lint: ## Lint files changed since main
	trunk check --upstream origin/main

lint-all: ## Lint all files
	trunk check -a

fmt: ## Format and fix files changed since main
	trunk fmt --upstream origin/main
	trunk check --upstream origin/main -y

fmt-all: ## Format and fix all files
	trunk fmt -a
	trunk check -a -y

# Backend commands
backend-dev: ## Run backend in development mode
	cd backend && uv run uvicorn mainloop.api:app --reload --host 0.0.0.0 --port 8000

# Frontend commands
frontend-dev: ## Run frontend in development mode
	cd frontend && pnpm dev

# Docker image commands
build-backend: ## Build backend Docker image
	docker build -f backend/Dockerfile -t $(BACKEND_IMAGE) .

build-frontend: ## Build frontend Docker image
	docker build -f frontend/Dockerfile -t $(FRONTEND_IMAGE) .

build-all: build-backend build-frontend ## Build all Docker images

push-backend: build-backend ## Push backend to GHCR
	docker push $(BACKEND_IMAGE)

push-frontend: build-frontend ## Push frontend to GHCR
	docker push $(FRONTEND_IMAGE)

push-all: push-backend push-frontend ## Push all images to GHCR

# Parallel build + push (much faster)
build-all-parallel: ## Build all Docker images in parallel
	@echo "Building all images in parallel..."
	@docker build -f backend/Dockerfile -t $(BACKEND_IMAGE) . & \
	docker build -f frontend/Dockerfile -t $(FRONTEND_IMAGE) . & \
	wait
	@echo "All builds complete"

push-all-parallel: build-all-parallel ## Build and push all images in parallel
	@echo "Pushing all images in parallel..."
	@docker push $(BACKEND_IMAGE) & \
	docker push $(FRONTEND_IMAGE) & \
	wait
	@echo "All pushes complete"

# Production is GitOps-only: Argo CD applies the prod overlay from Git and the images
# are pinned by digest in the deployment repository. There are no targets that apply to
# or restart anything in a production cluster. Build and push images with the
# build-*/push-* targets above, then record the digests in the deployment repository.

# Kind (local Kubernetes testing)
KIND_CLUSTER_NAME ?= mainloop-test

kind-create: ## Create Kind cluster for local testing
	@./scripts/kind/create-cluster.sh

kind-delete: ## Delete Kind cluster
	@kind delete cluster --name $(KIND_CLUSTER_NAME)

kind-load: ## Build and load images into Kind
	@./scripts/kind/load-images.sh

kind-secrets: ## Create K8s secrets from .env
	@./scripts/kind/create-secrets.sh

kind-deploy: ## Deploy mainloop to Kind
	@./scripts/kind/deploy.sh

kind-reset: dev-reset ## Alias for dev-reset

kind-logs: ## Tail backend logs (Kind test cluster)
	@kubectl --context=kind-$(KIND_CLUSTER_NAME) logs -n mainloop deployment/mainloop-backend -f

kind-shell: ## Open shell in backend pod (Kind test cluster)
	@kubectl --context=kind-$(KIND_CLUSTER_NAME) exec -it -n mainloop deployment/mainloop-backend -- bash

test-k8s: kind-create kind-load kind-secrets kind-deploy ## Start local K8s test environment
	@echo ""
	@echo "=== Local K8s environment ready ==="
	@echo "Frontend: $(TEST_FRONTEND_URL)"
	@echo "Backend:  $(TEST_API_URL)"
	@echo ""
	@echo "Run 'make kind-logs' to tail backend logs"
	@echo "Run 'make kind-reset' to reset data between tests"
	@echo "Run 'make test-loop' for auto-redeploy on changes"

test-loop: ## Watch for changes and auto-redeploy to Kind
	@echo "Starting Kind deploy loop (Ctrl+C to stop)..."
	@echo "Watching: backend/, frontend/src/"
	@trap 'kill 0' INT; \
	watchexec -w backend/src -w models -e py \
		--on-busy-update restart -- bash -c 'make kind-load && make kind-deploy' & \
	watchexec -w frontend/src -e ts,svelte,css \
		--on-busy-update restart -- bash -c 'make kind-load && make kind-deploy' & \
	wait

# =============================================================================
# Testing (DevSpace + Playwright)
#
# The Playwright and live-agent e2e suites are disabled by default and no
# longer run in CI. Set ENABLE_E2E=1 to run them explicitly.
# =============================================================================
TEST_API_URL := http://localhost:8081
TEST_FRONTEND_URL := http://localhost:5173

test-backend: ## Offline backend + scratch PostgreSQL; 30s/test, 9m/suite
	@cd backend && uv run --no-sync python scripts/test_backend.py $(TEST_ARGS)

test: ## Deploy to Kind + open Playwright UI (disabled; ENABLE_E2E=1 to opt in)
	@./scripts/e2e-guard.sh
	@./scripts/test-guard.sh
	devspace deploy --profile test --kube-context kind-$(KIND_CLUSTER_NAME) -n mainloop
	@echo "Waiting for backend..."
	@until curl -sf $(TEST_API_URL)/health > /dev/null 2>&1; do sleep 2; done
	@cd frontend && PLAYWRIGHT_BASE_URL=$(TEST_FRONTEND_URL) API_URL=$(TEST_API_URL) pnpm exec playwright test --ui

test-run: ## Run tests headless (disabled; ENABLE_E2E=1 to opt in)
	@./scripts/e2e-guard.sh
	@./scripts/wait-for-ready.sh
	@cd frontend && PLAYWRIGHT_BASE_URL=$(TEST_FRONTEND_URL) API_URL=$(TEST_API_URL) pnpm exec playwright test $(TEST_ARGS)

test-reset: dev-reset ## Alias for dev-reset

test-ci: ## Run tests with legacy kind scripts (disabled; ENABLE_E2E=1 to opt in)
	@./scripts/e2e-guard.sh
	@if ! kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		$(MAKE) kind-create; \
	fi
	@$(MAKE) kind-load
	@$(MAKE) kind-secrets
	@$(MAKE) kind-deploy
	@until curl -sf $(TEST_API_URL)/health > /dev/null 2>&1; do sleep 2; done
	@cd frontend && PLAYWRIGHT_BASE_URL=$(TEST_FRONTEND_URL) API_URL=$(TEST_API_URL) pnpm exec playwright test

# Debugging commands
# Set API_URL in .env file or override: make debug-tasks API_URL=https://your-api.example.com
API_URL ?= https://mainloop-api.example.com

debug-tasks: ## Show all tasks with workflow status
	@curl -s $(API_URL)/debug/tasks | jq '.[] | {id: .task.id, status: .task.status, workflow: .workflow_status, error: .workflow_error, namespace: .namespace_exists, pr: .task.pr_url}'

debug-task: ## Show detailed info for a specific task (usage: make debug-task TASK_ID=xxx)
	@curl -s $(API_URL)/debug/tasks | jq '.[] | select(.task.id | startswith("$(TASK_ID)"))'

debug-retry: ## Retry a failed task (usage: make debug-retry TASK_ID=xxx)
	@curl -s -X POST $(API_URL)/debug/tasks/$(TASK_ID)/retry | jq

debug-logs: ## Show backend logs
	kubectl logs -n mainloop deployment/mainloop-backend --tail=100 -f

debug-db: ## Query tasks directly from database
	@kubectl exec -n mainloop deployment/mainloop-backend -- python3 -c "\
import asyncio; \
from mainloop.db import db; \
async def q(): \
    await db.connect(); \
    async with db.connection() as c: \
        rows = await c.fetch('SELECT id, status, pr_url, error FROM worker_tasks ORDER BY created_at DESC LIMIT 5'); \
        for r in rows: print(dict(r)); \
    await db.disconnect(); \
asyncio.run(q())"
