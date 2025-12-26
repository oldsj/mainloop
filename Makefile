.PHONY: help dev build deploy deploy-frontend build-backend push-backend install clean setup-claude-creds setup-claude-creds-k8s

# Configuration
GHCR_REGISTRY := ghcr.io
GHCR_USER := oldsj
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

# Deployment
deploy-frontend: ## Deploy frontend to Cloudflare Pages
	cd frontend && pnpm build && npx wrangler deploy --env production

deploy: push-all ## Full deployment to k8s
	@echo "Applying Kubernetes manifests..."
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side
	@echo "Restarting Kubernetes deployments..."
	kubectl rollout restart deployment/mainloop-backend -n mainloop
	kubectl rollout restart deployment/mainloop-agent-controller -n mainloop
	kubectl rollout restart deployment/mainloop-frontend -n mainloop

# K8s commands (for local testing before moving to infrastructure repo)
k8s-apply: ## Apply K8s manifests locally
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side

k8s-delete: ## Delete K8s resources
	kubectl delete -k k8s/apps/mainloop/overlays/prod
