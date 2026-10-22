.DEFAULT_GOAL := help

BINARY      := sorovault
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

# Port 55433 keeps the test database clear of the compose stack on 5432, so
# `make test-all` does not disturb a running dev environment.
TEST_PG_PORT      ?= 55433
TEST_PG_CONTAINER := sorovault-test-pg
TEST_DATABASE_URL ?= postgres://sorovault:sorovault@localhost:$(TEST_PG_PORT)/sorovault?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test: ## Run the tests (no database or network needed)
	go test -race ./...

.PHONY: test-all
test-all: ## Run the tests including the Postgres store suite
	@docker rm -f $(TEST_PG_CONTAINER) >/dev/null 2>&1 || true
	@docker run -d --rm --name $(TEST_PG_CONTAINER) \
		-e POSTGRES_USER=sorovault -e POSTGRES_PASSWORD=sorovault -e POSTGRES_DB=sorovault \
		-p $(TEST_PG_PORT):5432 postgres:16-alpine >/dev/null
	@echo "waiting for postgres..."
	@for i in $$(seq 1 30); do \
		docker exec $(TEST_PG_CONTAINER) pg_isready -U sorovault >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	@TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race ./...; \
		status=$$?; docker rm -f $(TEST_PG_CONTAINER) >/dev/null 2>&1; exit $$status

.PHONY: cover
cover: ## Write a coverage profile and print the total
	go test -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Vet and check formatting
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: check
check: lint test ## Vet, check formatting and test — run before opening a PR

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: golden
golden: ## Regenerate the golden ABI fixture, the TypeScript client built from it and the JSON schemas
	go test ./internal/spec -run TestDecodeFixture -update
	go test ./internal/codegen -run TestGoldenZKVote -update
	go test ./internal/codegen -run TestGoldenZKVoteSchemas -update

.PHONY: up
up: ## Start Postgres, run migrations and serve, via docker compose
	docker compose up --build

.PHONY: down
down: ## Stop the compose stack
	docker compose down

.PHONY: clean-data
clean-data: ## Stop the compose stack and delete its database volume
	docker compose down -v

.PHONY: migrate
migrate: ## Apply migrations to $$DATABASE_URL
	go run ./cmd/$(BINARY) migrate

.PHONY: run
run: ## Run the server against $$DATABASE_URL
	go run ./cmd/$(BINARY) serve

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out
