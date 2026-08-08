SHELL       := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE      := github.com/thulasiram/oto
BIN_DIR     := bin
BINARY      := $(BIN_DIR)/oto
WEB_DIR     := web
MIGRATIONS  := db/migrations

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.Commit=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'

# Reads OTO_DB_URL from the environment or .env, falling back to the compose defaults.
DB_URL      ?= $(or $(OTO_DB_URL),postgres://oto:oto@localhost:5432/oto?sslmode=disable)
GOOSE       := go run github.com/pressly/goose/v3/cmd/goose@v3.27.3 -dir $(MIGRATIONS) postgres "$(DB_URL)"

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- development

.PHONY: dev
dev: ## Run the API with live config from .env (requires db-up)
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/oto

.PHONY: build
build: ## Build the oto binary into ./bin
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/oto
	@echo "built $(BINARY) $(VERSION)"

.PHONY: fmt
fmt: ## Format Go sources and tidy go.mod
	go fmt ./...
	go mod tidy

.PHONY: generate
generate: ## Run go generate and regenerate the TypeScript API client
	go generate ./...
	@echo "note: the TS client is generated from api/openapi/openapi.yaml (C20)"

## -------------------------------------------------------------------- quality

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted or go.mod is untidy (what CI checks)
	@unformatted="$$(gofmt -l ./cmd ./internal ./pkg ./db ./test ./tools)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt: $$unformatted"; exit 1; fi
	go mod tidy
	git diff --exit-code -- go.mod go.sum

.PHONY: vet
vet: ## go vet the whole tree
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (installs it on first use)
	@command -v golangci-lint >/dev/null 2>&1 \
		|| { echo "installing golangci-lint..."; go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; }
	golangci-lint run ./...

.PHONY: test
test: ## Run the Go test suite (integration tests need Docker)
	go test -race -count=1 ./...

.PHONY: lint-vocabulary
lint-vocabulary: ## SCOPE-BOUNDARY AC-49: no on-call vocabulary in internal/, web/src/, db/migrations/
	go run ./tools/lintvocab

.PHONY: generate-check
generate-check: ## Gate G3 (SPEC §L.8.1): the checked-in TS client must match openapi.yaml
	cd $(WEB_DIR) && npm run generate:check

.PHONY: ci
ci: fmt-check lint vet lint-vocabulary generate-check build test ui-build ui-test ## Everything CI runs, in order

## ------------------------------------------------------------------- database

.PHONY: db-up
db-up: ## Start Postgres and Alertmanager, waiting for health
	docker compose up -d --wait postgres alertmanager

.PHONY: db-down
db-down: ## Stop the dev stack (keeps the volume)
	docker compose down

.PHONY: db-reset
db-reset: ## Destroy the dev database volume and start clean
	docker compose down -v
	$(MAKE) db-up
	$(MAKE) migrate-up

.PHONY: migrate-up
migrate-up: ## Apply every pending migration
	$(GOOSE) up

.PHONY: migrate-down
migrate-down: ## Roll back exactly one migration
	$(GOOSE) down

.PHONY: migrate-status
migrate-status: ## Show which migrations have been applied
	$(GOOSE) status

.PHONY: migrate-create
migrate-create: ## Create a migration: make migrate-create name=add_alerts
	@test -n "$(name)" || { echo "usage: make migrate-create name=<snake_case>"; exit 1; }
	$(GOOSE) create $(name) sql

## ------------------------------------------------------------------------- ui

.PHONY: ui-install
ui-install: ## Install UI dependencies
	cd $(WEB_DIR) && npm install

.PHONY: ui-dev
ui-dev: ## Run the Vite dev server (proxies /api and /healthz to :8080)
	cd $(WEB_DIR) && npm run dev

.PHONY: ui-build
ui-build: ## Typecheck and build the UI
	cd $(WEB_DIR) && npm ci --no-audit --no-fund && npm run build

.PHONY: ui-test
ui-test: ## Run the UI test suite
	cd $(WEB_DIR) && npm run test

## ---------------------------------------------------------------------- other

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BIN_DIR) $(WEB_DIR)/dist
