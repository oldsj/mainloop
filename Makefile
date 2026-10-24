.PHONY: help dev dev-stop dev-reset kind-up kind-create kind-delete kind-load kind-secrets kind-deploy kind-forward kind-reset kind-logs kind-shell dev-live dev-live-status dev-live-purge dev-agents dev-harness dev-live-lease-acquire dev-live-lease-release install check check-mcp-tools check-server check-web lint lint-all fmt fmt-all _fmt _fmt-all test-timeouts test-timeout-integration

# Load .env file if it exists
-include .env
export

# Shared foreground caps; setup and checks never fetch a timeout runtime.
CAP := node "$(CURDIR)/scripts/with-timeout.mjs"

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# =============================================================================
# Local Kind stack (DevSpace profile in devspace.yaml; see docs/dev-environment.md#kind)
# =============================================================================
KIND_CLUSTER_NAME ?= mainloop-local
KIND_CONTEXT = kind-$(KIND_CLUSTER_NAME)

dev: ## Build, load and deploy the Kind stack, forward its ports and stream logs (DevSpace)
	devspace dev --kube-context $(KIND_CONTEXT) -n mainloop

dev-stop: ## Stop DevSpace and delete the mainloop namespace, its database included (the cluster stays)
	devspace purge --kube-context $(KIND_CONTEXT) -n mainloop

dev-reset: ## Wipe the Kind database and restart the backend
	@./scripts/kind/reset-data.sh

# =============================================================================
# Shared live instance (DevSpace profile `live`, namespace mainloop-dev; needs DEV_LIVE_CONTEXT and MAINLOOP_DEV_CONFIG)
# =============================================================================

dev-live: ## Deploy mainloop-dev and live-sync the Go server and web app into it
	./scripts/dev-live.sh dev

dev-live-status: ## Show mainloop-dev pods and the dev kagent agents
	./scripts/dev-live.sh status

dev-live-purge: ## Delete namespace mainloop-dev and the *-dev kagent agents
	./scripts/dev-live.sh purge

dev-agents: ## Apply the *-dev kagent agents (shared kagent namespace)
	./scripts/dev-live.sh agents

dev-live-lease-acquire: ## Acquire the devloop lease as the owner (needs MAINLOOP_API_URL)
	./scripts/dev-live.sh lease-acquire

dev-live-lease-release: ## Release the devloop lease as the owner (needs MAINLOOP_API_URL)
	./scripts/dev-live.sh lease-release

dev-harness: ## Build arm64 harness images from ~/dev/kagent and point the dev agents at them
	./scripts/dev-live-harness.sh build

kind-up: kind-create kind-load kind-secrets kind-deploy ## Create the cluster, build and load images, deploy (no DevSpace)

kind-create: ## Create the Kind cluster (skipped if it exists)
	@./scripts/kind/create-cluster.sh

kind-delete: ## Delete the Kind cluster and all its data
	@kind delete cluster --name $(KIND_CLUSTER_NAME)

kind-load: ## Build the server and web images and load them into Kind
	@./scripts/kind/load-images.sh

kind-secrets: ## Create mainloop-secrets with random local values (kept if present)
	@./scripts/kind/create-secrets.sh

kind-deploy: ## Apply the Kind overlay and wait until it is ready
	@./scripts/kind/deploy.sh

kind-forward: ## Forward the app to http://localhost:5173 and the API to localhost:8000 (Ctrl-C stops both)
	@trap 'kill 0' INT TERM; \
	kubectl --context $(KIND_CONTEXT) -n mainloop port-forward svc/mainloop-frontend 5173:3000 & \
	kubectl --context $(KIND_CONTEXT) -n mainloop port-forward svc/mainloop-backend 8000:8000 & \
	wait

kind-reset: dev-reset ## Alias for dev-reset

kind-logs: ## Tail backend logs (Kind)
	@kubectl --context $(KIND_CONTEXT) -n mainloop logs deployment/mainloop-backend -c backend -f

kind-shell: ## Open a shell in the Kind backend container
	@kubectl --context $(KIND_CONTEXT) -n mainloop exec -it deployment/mainloop-backend -c backend -- sh

# =============================================================================
# Dependencies and checks
# =============================================================================

install: ## Install locked workspace dependencies (240s cap)
	@$(CAP) 240 'make install' -- pnpm install --frozen-lockfile

check: ## All offline checks: MCP contract, Go build and vet, web (installs dependencies; 480s cap)
	@$(CAP) 480 'make check' -- $(MAKE) --no-print-directory check-mcp-tools check-server check-web

check-mcp-tools: ## api/mcp-tools.json matches the Go tool registry and policy table (240s cap)
	@$(CAP) 240 'make check-mcp-tools' -- sh -c 'cd server && go run ./tools/mcpcontract -check ../api/mcp-tools.json'

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
