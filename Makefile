.PHONY: help dev build deploy deploy-loop deploy-loop-all deploy-frontend deploy-backend deploy-agent deploy-frontend-k8s deploy-manifests build-backend push-backend build-all-parallel push-all-parallel install clean setup-claude-creds setup-claude-creds-k8s debug-tasks debug-task debug-retry debug-logs debug-db

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
AGENT_CONTROLLER_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-agent-controller:$(IMAGE_TAG)

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# Development commands
dev: ## Start all services with hot reload
	docker compose up --build --watch

install: ## Install all dependencies
	pnpm install
	cd backend && uv sync
	cd models && uv sync

clean: ## Clean build artifacts
	rm -rf frontend/.svelte-kit frontend/build
	rm -rf backend/.venv models/.venv
	find . -type d -name "__pycache__" -exec rm -rf {} +

setup-claude-creds: ## Login to Claude inside Linux container, credentials saved to shared volume
	@echo "=== Claude Container Login ==="
	@echo "This will start a container where you can login to Claude."
	@echo "Credentials will be saved to the 'claude-config' Docker volume."
	@echo ""
	@docker build -q -t mainloop-claude-agent ./claude-agent > /dev/null
	@docker volume create claude-config > /dev/null 2>&1 || true
	@docker rm -f claude-login-tmp > /dev/null 2>&1 || true
	@docker run -d --entrypoint "" --name claude-login-tmp \
		-v claude-config:/home/claude/.claude \
		mainloop-claude-agent sleep 3600 > /dev/null
	@echo "Container started with shared claude-config volume."
	@echo "Run: claude login"
	@echo "Complete the browser OAuth flow, then type 'exit'"
	@echo ""
	@docker exec -it claude-login-tmp bash; \
	echo ""; \
	echo "Checking credentials..."; \
	if docker exec claude-login-tmp test -f /home/claude/.claude/.credentials.json; then \
		echo "✓ Credentials saved to claude-config volume"; \
		echo "  All containers mounting this volume will have access."; \
	else \
		echo "⚠ No credentials found. Did you complete the login?"; \
	fi; \
	docker rm -f claude-login-tmp > /dev/null

setup-claude-creds-mac: ## Extract Claude credentials from macOS Keychain (Mac only)
	@echo "Extracting Claude credentials from macOS Keychain..."
	@CREDS=$$(security find-generic-password -s "Claude Code-credentials" -a "$(USER)" -w 2>/dev/null); \
	if [ -z "$$CREDS" ]; then \
		echo "Error: Claude credentials not found in Keychain."; \
		echo "Make sure you're logged in to Claude Code on this Mac."; \
		exit 1; \
	fi; \
	if [ -f .env ]; then \
		echo "Updating CLAUDE_CREDENTIALS in .env (preserving other variables)..."; \
		grep -v "^CLAUDE_CREDENTIALS=" .env > .env.tmp || true; \
		mv .env.tmp .env; \
	else \
		echo "Creating .env file..."; \
		touch .env; \
	fi; \
	echo "CLAUDE_CREDENTIALS=$$CREDS" >> .env; \
	echo "✓ Claude credentials updated in .env"

setup-claude-creds-k8s: ## Push Claude credentials from Docker volume to 1Password for k8s
	@echo "Extracting credentials from Docker volume and pushing to 1Password..."
	@if ! command -v op >/dev/null 2>&1; then \
		echo "Error: 1Password CLI (op) not found."; \
		echo "Install it: brew install --cask 1password-cli"; \
		exit 1; \
	fi; \
	docker run --rm -v claude-config:/config:ro alpine cat /config/.credentials.json > /tmp/claude-creds.json 2>/dev/null; \
	if [ ! -s /tmp/claude-creds.json ]; then \
		echo "Error: No credentials in claude-config volume"; \
		echo "Run 'make setup-claude-creds' first"; \
		rm -f /tmp/claude-creds.json; \
		exit 1; \
	fi; \
	echo "Creating/updating claude-credentials item in kubernetes vault..."; \
	op item get claude-credentials --vault kubernetes >/dev/null 2>&1 && \
		op item delete claude-credentials --vault kubernetes || true; \
	op document create /tmp/claude-creds.json --title=claude-credentials --vault=kubernetes >/dev/null; \
	rm -f /tmp/claude-creds.json; \
	echo "✓ Claude credentials pushed to 1Password vault 'kubernetes'"

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

build-agent-controller: ## Build agent controller Docker image
	docker build -f claude-agent/Dockerfile -t $(AGENT_CONTROLLER_IMAGE) ./claude-agent

build-all: build-backend build-frontend build-agent-controller ## Build all Docker images

push-backend: build-backend ## Push backend to GHCR
	docker push $(BACKEND_IMAGE)

push-frontend: build-frontend ## Push frontend to GHCR
	docker push $(FRONTEND_IMAGE)

push-agent-controller: build-agent-controller ## Push agent controller to GHCR
	docker push $(AGENT_CONTROLLER_IMAGE)

push-all: push-backend push-frontend push-agent-controller ## Push all images to GHCR

# Parallel build + push (much faster)
build-all-parallel: ## Build all Docker images in parallel
	@echo "Building all images in parallel..."
	@docker build -f backend/Dockerfile -t $(BACKEND_IMAGE) . & \
	docker build -f frontend/Dockerfile -t $(FRONTEND_IMAGE) . & \
	docker build -f claude-agent/Dockerfile -t $(AGENT_CONTROLLER_IMAGE) ./claude-agent & \
	wait
	@echo "All builds complete"

push-all-parallel: build-all-parallel ## Build and push all images in parallel
	@echo "Pushing all images in parallel..."
	@docker push $(BACKEND_IMAGE) & \
	docker push $(FRONTEND_IMAGE) & \
	docker push $(AGENT_CONTROLLER_IMAGE) & \
	wait
	@echo "All pushes complete"

# Deployment
deploy: push-all-parallel ## Full deployment to k8s (parallel builds + pushes)
	@echo "Applying Kubernetes manifests..."
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side --force-conflicts
	@echo "Restarting Kubernetes deployments in parallel..."
	@kubectl rollout restart deployment/mainloop-backend -n mainloop & \
	kubectl rollout restart deployment/mainloop-agent-controller -n mainloop & \
	kubectl rollout restart deployment/mainloop-frontend -n mainloop & \
	wait
	@echo "Rollouts triggered"

deploy-loop: ## Smart watch - detects which service changed and deploys only that
	@echo "Starting smart deploy loop (Ctrl+C to stop)..."
	@echo "Watching: backend/ models/ -> deploy-backend"
	@echo "Watching: frontend/src/ -> deploy-frontend-k8s"
	@echo "Watching: claude-agent/ -> deploy-agent"
	@echo "Watching: k8s/ -> deploy-manifests"
	@trap 'kill 0' INT; \
	watchexec -w backend -w models -e py,toml \
		-i 'test*' -i '*_test.py' -i 'tests/' -i '__pycache__/' -i 'scripts/' \
		--on-busy-update restart -- $(MAKE) deploy-backend & \
	watchexec -w frontend/src -e ts,svelte,css \
		--on-busy-update restart -- $(MAKE) deploy-frontend-k8s & \
	watchexec -w claude-agent -e py,Dockerfile,toml \
		-i 'test*' \
		--on-busy-update restart -- $(MAKE) deploy-agent & \
	watchexec -w k8s -e yaml \
		--on-busy-update restart -- $(MAKE) deploy-manifests & \
	wait

deploy-loop-all: ## Watch all and redeploy everything (old behavior)
	watchexec --poll 1000 -w backend -w frontend/src -w k8s -w models -w claude-agent \
		-e py,ts,svelte,yaml,toml,Dockerfile \
		-i 'test*' -i '*_test.py' -i 'tests/' -i '__pycache__/' -i '.pytest_cache/' -i 'scripts/' \
		--on-busy-update restart \
		-- $(MAKE) deploy || true

# Selective deploy targets (faster - skip kubectl apply, just build+push+restart)
deploy-backend: ## Build, push, and restart backend only
	docker build -f backend/Dockerfile -t $(BACKEND_IMAGE) .
	docker push $(BACKEND_IMAGE)
	kubectl rollout restart deployment/mainloop-backend -n mainloop

deploy-agent: ## Build, push, and restart agent controller only
	docker build -f claude-agent/Dockerfile -t $(AGENT_CONTROLLER_IMAGE) ./claude-agent
	docker push $(AGENT_CONTROLLER_IMAGE)
	kubectl rollout restart deployment/mainloop-agent-controller -n mainloop

deploy-frontend-k8s: ## Build, push, and restart frontend only (k8s version)
	docker build -f frontend/Dockerfile -t $(FRONTEND_IMAGE) .
	docker push $(FRONTEND_IMAGE)
	kubectl rollout restart deployment/mainloop-frontend -n mainloop

deploy-manifests: ## Apply k8s manifests only (no image builds)
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side --force-conflicts

# K8s commands (for local testing before moving to infrastructure repo)
k8s-apply: ## Apply K8s manifests locally
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side

k8s-delete: ## Delete K8s resources
	kubectl delete -k k8s/apps/mainloop/overlays/prod

# E2E Testing
test-k8s: ## Test K8s namespace/secret creation (quick)
	cd backend && uv run python scripts/test_k8s_components.py

test-k8s-job: ## Test K8s job creation (creates a real job)
	cd backend && uv run python scripts/test_k8s_components.py --job

test-worker-e2e: ## Run full worker E2E test (requires running backend + k8s)
	cd backend && REPO_URL="$(or $(REPO_URL),https://github.com/oldsj/mainloop)" uv run python scripts/test_worker_e2e.py

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
