.PHONY: help dev build deploy deploy-frontend build-backend push-backend install clean setup-claude-creds

# Configuration
GHCR_REGISTRY := ghcr.io
GHCR_USER := oldsj
IMAGE_TAG ?= latest
BACKEND_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-backend:$(IMAGE_TAG)
FRONTEND_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-frontend:$(IMAGE_TAG)
CLAUDE_IMAGE := $(GHCR_REGISTRY)/$(GHCR_USER)/mainloop-claude-agent:$(IMAGE_TAG)

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

setup-claude-creds: ## Extract Claude credentials from macOS Keychain and update .env
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
	docker build -f frontend/Dockerfile -t $(FRONTEND_IMAGE) ./frontend

build-claude-agent: ## Build Claude agent Docker image
	docker build -f claude-agent/Dockerfile -t $(CLAUDE_IMAGE) ./claude-agent

build-all: build-backend build-frontend build-claude-agent ## Build all Docker images

push-backend: build-backend ## Push backend to GHCR
	docker push $(BACKEND_IMAGE)

push-frontend: build-frontend ## Push frontend to GHCR
	docker push $(FRONTEND_IMAGE)

push-claude-agent: build-claude-agent ## Push Claude agent to GHCR
	docker push $(CLAUDE_IMAGE)

push-all: push-backend push-frontend push-claude-agent ## Push all images to GHCR

# Deployment
deploy-frontend: ## Deploy frontend to Cloudflare Pages
	cd frontend && pnpm build && npx wrangler deploy --env production

deploy: push-all deploy-frontend ## Full deployment
	@echo "Restarting Kubernetes deployments..."
	kubectl rollout restart deployment/mainloop-backend -n mainloop
	kubectl rollout restart deployment/mainloop-claude-agent -n mainloop

# K8s commands (for local testing before moving to infrastructure repo)
k8s-apply: ## Apply K8s manifests locally
	kubectl apply -k k8s/apps/mainloop/overlays/prod --server-side

k8s-delete: ## Delete K8s resources
	kubectl delete -k k8s/apps/mainloop/overlays/prod
