GO ?= go
MIGRATE_DIR := internal/platform/database/migrations

.PHONY: run build test tidy fmt vet up down logs migrate-create

run: ## Run the server locally (needs DATABASE_URL + JWT_SECRET in env)
	$(GO) run ./cmd/server

build: ## Compile the server binary into ./bin
	$(GO) build -o bin/server ./cmd/server

test: ## Run all tests
	$(GO) test ./...

tidy: ## Sync go.mod / go.sum
	$(GO) mod tidy

fmt: ## Format all code
	$(GO) fmt ./...

vet: ## Static checks
	$(GO) vet ./...

up: ## Build & start app + postgres via docker compose
	docker compose up -d --build

down: ## Stop and remove containers
	docker compose down

logs: ## Tail the app logs
	docker compose logs -f app

# Usage: make migrate-create name=add_posts
migrate-create: ## Scaffold a new up/down migration pair
	@test -n "$(name)" || (echo "usage: make migrate-create name=<description>"; exit 1)
	@next=$$(printf "%06d" $$(( $$(ls $(MIGRATE_DIR) 2>/dev/null | grep -oE '^[0-9]+' | sort -n | tail -1 | sed 's/^0*//') + 1 ))); \
	touch $(MIGRATE_DIR)/$${next}_$(name).up.sql $(MIGRATE_DIR)/$${next}_$(name).down.sql; \
	echo "created $(MIGRATE_DIR)/$${next}_$(name).{up,down}.sql"
