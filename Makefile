.PHONY: help dev dev-stop dev-reset dev-clear-cache dev-logs dev-shell kind-create kind-delete kind-load kind-secrets kind-deploy kind-reset kind-logs kind-shell test-k8s test-loop test-reset dev-live dev-live-status dev-live-purge dev-agents dev-harness install check check-mcp-tools check-server check-web lint lint-all fmt fmt-all _fmt _fmt-all test-timeouts test-timeout-integration

# Load .env file if it exists
-include .env
export

# Shared foreground caps; setup and checks never fetch a timeout runtime.
CAP := node "$(CURDIR)/scripts/with-timeout.mjs"

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

# =============================================================================
# Shared live instance (DevSpace profile `live`, namespace mainloop-dev on internal-01)
# =============================================================================

dev-live: ## Deploy mainloop-dev and live-sync the Go server and web app into it
	./scripts/dev-live.sh dev

dev-live-status: ## Show mainloop-dev pods and the dev kagent agents
	./scripts/dev-live.sh status

dev-live-purge: ## Delete namespace mainloop-dev and the *-dev kagent agents
	./scripts/dev-live.sh purge

dev-agents: ## Apply the *-dev kagent agents (shared kagent namespace)
	./scripts/dev-live.sh agents

dev-harness: ## Build arm64 harness images from ~/dev/kagent and point the dev agents at them
	./scripts/dev-live-harness.sh build

# =============================================================================
# Kind (local Kubernetes for trying Mainloop locally)
# =============================================================================
KIND_CLUSTER_NAME ?= mainloop-test
TEST_API_URL := http://localhost:8081
TEST_FRONTEND_URL := http://localhost:5173

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

test-reset: dev-reset ## Alias for dev-reset

# =============================================================================
# Dependencies and checks
# =============================================================================

install: ## Install locked workspace dependencies (240s cap)
	@$(CAP) 240 'make install' -- pnpm install --frozen-lockfile

check: ## All offline checks: MCP contract, Go build and vet, web (installs dependencies; 480s cap)
	@$(CAP) 480 'make check' -- $(MAKE) --no-print-directory check-mcp-tools check-server check-web

check-mcp-tools: ## api/mcp-tools.json matches the Python exporter and the Go tool registry (240s cap; installs backend dependencies)
	@$(CAP) 240 'make check-mcp-tools' -- sh -c 'uv run --frozen --project backend --python 3.13 python scripts/export-mcp-tools.py --check && cd server && go test -count=1 -run "TestToolSetMatchesTheContract|TestVisibilityMatchesTheContract|TestInputSchemasMatchTheContract|TestSchemaComparisonSeesADifferentType" ./internal/mcpserver/'

check-server: ## Go build and vet for server/ (240s cap)
	@$(CAP) 240 'make check-server' -- sh -c 'cd server && go build ./... && go vet ./...'

check-web: install ## Web type check, lint and unit tests (240s cap)
	@$(CAP) 240 'make check-web' -- sh -c 'cd web && node_modules/.bin/tsc -b && node_modules/.bin/eslint . && node_modules/.bin/vitest run'

lint: ## Lint files changed since main
	@$(CAP) 120 'make lint' -- trunk check --upstream origin/main

lint-all: ## Lint all files
	@$(CAP) 240 'make lint-all' -- trunk check -a

fmt: ## Format and fix files changed since main
	@$(CAP) 180 'make fmt' -- $(MAKE) --no-print-directory _fmt

_fmt:
	trunk fmt --upstream origin/main
	trunk check --upstream origin/main -y

fmt-all: ## Format and fix all files
	@$(CAP) 300 'make fmt-all' -- $(MAKE) --no-print-directory _fmt-all

_fmt-all:
	trunk fmt -a
	trunk check -a -y

test-timeouts: ## Command deadline helper regression tests (90s cap)
	@$(CAP) 90 'make test-timeouts' -- node --test scripts/with-timeout.test.mjs

test-timeout-integration: ## Opt-in dev-image PG16 cleanup regression (needs MAINLOOP_TIMEOUT_TEST_IMAGE; 180s cap)
	@$(CAP) 180 'make test-timeout-integration' -- node --test scripts/with-timeout.integration.test.mjs

# Production is GitOps-only: Argo CD applies the prod overlay from Git, and no target here
# applies to or restarts a production cluster. CI builds and publishes the images.
