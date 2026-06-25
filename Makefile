GO ?= go
MIGRATE_DIR := internal/platform/database/migrations

.PHONY: run build test tidy fmt vet up down logs migrate migrate-create

run: ## Run the server locally (needs DATABASE_URL + JWT_SECRET in env)
	$(GO) run ./cmd/server

build: ## Compile the server binary into ./bin
	$(GO) build -o bin/server ./cmd/server

migrate: ## Apply all pending migrations as a standalone step (needs DATABASE_URL)
	$(GO) run ./cmd/migrate

test: ## Run unit tests (no DB required)
	$(GO) test -cover ./...

test-integration: ## Run unit + integration tests against the dedicated tsz_test DB
	@docker exec tsz-go-db-1 psql -U app -d postgres -tc \
		"SELECT 1 FROM pg_database WHERE datname='tsz_test'" | grep -q 1 \
		|| docker exec tsz-go-db-1 psql -U app -d postgres -c "CREATE DATABASE tsz_test"
	DATABASE_URL="$${DATABASE_URL:-postgres://app:app@localhost:5432/tsz_test?sslmode=disable}" \
		$(GO) test -tags=integration -cover ./...

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
