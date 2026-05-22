# atlantis Makefile

SHELL := /bin/bash

# Load .env if present; export so recipes inherit.
-include .env
export

PG_URL ?= postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable

# Two migration histories: infra (hand-written) and tidectl (codegen).
MIGRATIONS_INFRA_DIR := ./migrations/infra
MIGRATIONS_TIDECTL_DIR := ./.dev/migrations/tidectl
MIGRATE_URL_INFRA := $(PG_URL)&x-migrations-table=atlantis_schema_migrations_infra
MIGRATE_URL_TIDECTL := $(PG_URL)&x-migrations-table=atlantis_schema_migrations_tidectl

GO ?= go
GOFLAGS ?=

BIN_DIR := ./bin

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-22s %s\n", $$1, $$2}'

# ---------- build ----------

.PHONY: build
build: build-server build-tidectl build-tide ## Build all binaries

.PHONY: build-server
build-server: ## Build the gRPC server
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/atlantis ./cmd/server

.PHONY: build-tidectl
build-tidectl: ## Build the server-side admin CLI
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/tidectl ./cmd/tidectl

.PHONY: build-tide
build-tide: ## Build the caller-side CLI
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/tide ./cmd/tide

# ---------- codegen ----------

.PHONY: codegen
codegen: ## Regenerate proto, server, client, sql from .atl; then buf generate; then build
	$(GO) run ./cmd/tidectl codegen
	@$(MAKE) proto
	# Build SDK in isolation so any leak from clients/go/ into internal/ fails here.
	cd clients/go && $(GO) build ./...
	# gen/ is optional on fresh clones (no .atl fixtures → no generated code).
	$(GO) build ./cmd/tide ./cmd/tidectl ./internal/...
	# Check for actual .go files, not just dir existence — an empty
	# leftover gen/ from a previous run would otherwise trip the build.
	@if [ -n "$$(find gen -type f -name '*.go' 2>/dev/null)" ]; then \
		$(GO) build ./gen/... ./cmd/server; \
	fi

.PHONY: proto
proto: ## Run buf generate against the regenerated .proto tree
	@which buf >/dev/null || (echo "install buf: brew install bufbuild/buf/buf" && exit 1)
	buf lint
	buf generate

.PHONY: plan
plan: ## Stage a migration from current .atl state
	$(GO) run ./cmd/tidectl plan

.PHONY: approve
approve: ## Promote staged migration into migrations/
	$(GO) run ./cmd/tidectl approve

# ---------- migrate ----------
#
# Two histories: infra first (the outbox + tidectl bookkeeping the rest of
# the system depends on), then tidectl (the codegen-emitted entity tables).

.PHONY: migrate-up
migrate-up: migrate-up-infra migrate-up-tidectl ## Apply all pending migrations (infra then tidectl)

.PHONY: migrate-up-infra
migrate-up-infra: ## Apply pending infra migrations
	@which migrate >/dev/null || (echo "install golang-migrate: brew install golang-migrate" && exit 1)
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up

.PHONY: migrate-up-tidectl
migrate-up-tidectl: ## Apply pending tidectl-emitted migrations
	migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent tidectl migration
	migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" down 1

.PHONY: migrate-down-infra
migrate-down-infra: ## Roll back the most recent infra migration
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" down 1

.PHONY: migrate-status
migrate-status: ## Show migration versions (both histories)
	@echo "infra:"
	@migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" version
	@echo "tidectl:"
	@migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" version

.PHONY: migrate-create-infra
migrate-create-infra: ## Create a blank infra migration: make migrate-create-infra NAME=description
	@test -n "$(NAME)" || (echo "NAME=description required" && exit 1)
	migrate create -ext sql -dir $(MIGRATIONS_INFRA_DIR) -seq $(NAME)

# ---------- test ----------

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-integration
test-integration: ## Run integration tests (testcontainers PG + memcached fake)
	$(GO) test -tags=integration ./tests/integration/...

.PHONY: test-codegen-golden
test-codegen-golden: ## Run codegen golden-file tests
	$(GO) test ./internal/codegen/...

# ---------- dev ----------

.PHONY: dev
dev: ## Run the server locally against docker-compose stack
	docker compose up -d postgres memcached
	AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		$(GO) run ./cmd/server

.PHONY: dev-isolated
dev-isolated: ## Full local stack via docker-compose (server + pg + memcached)
	# --build: rebuild atlantis image so a stale one isn't reused.
	# --profile isolated: opt into the atlantis service (otherwise infra-only).
	docker compose --profile isolated up --build

.PHONY: dev-down
dev-down: ## Tear down the docker-compose stack
	docker compose --profile isolated down -v

# dev-tree: symlink real infra migrations + create empty tidectl dir for dev-build's staged plans.
.PHONY: dev-tree
dev-tree:
	@mkdir -p .dev/migrations/tidectl
	@test -L .dev/migrations/infra || ln -sfn ../../migrations/infra .dev/migrations/infra

.PHONY: dev-watch
dev-watch: dev-tree ## Hot-reload server on .atl / .go edits (installs air if missing)
	@which air >/dev/null 2>&1 || (echo "==> installing air (one-time)..." && $(GO) install github.com/air-verse/air@latest)
	@echo "==> watching testdata/schema/, cmd/, internal/ — Ctrl-C to stop"
	@AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		PG_URL="$(PG_URL)" \
		MEMCACHED_ADDR="$${MEMCACHED_ADDR:-localhost:11211}" \
		LOG_LEVEL=debug \
		MIGRATIONS_DIR=./.dev/migrations \
		air

# dev-build cycle: plan (stage diff if any) → approve (only if non-empty) → codegen → build.
# plan runs before codegen so the .atl-vs-checkpoint diff is visible.
.PHONY: dev-build
dev-build: ## One dev cycle (called by air): plan → approve (if meaningful) → codegen → build
	@printf "\033[36m==> plan\033[0m\n"
	-@$(GO) run ./cmd/tidectl plan -schema-dir=testdata/schema -migrations-dir=.dev/migrations/tidectl -ir-checkpoint=gen/.last-ir.json -stage-dir=.dev/migrations/tidectl/_staged
	@if ls .dev/migrations/tidectl/_staged/*.up.sql >/dev/null 2>&1; then \
		if grep -q "(no schema changes)" .dev/migrations/tidectl/_staged/*.up.sql; then \
			printf "\033[90m==> approve skipped (no schema diff)\033[0m\n"; \
			rm -f .dev/migrations/tidectl/_staged/*.sql; \
		else \
			printf "\033[36m==> approve\033[0m\n"; \
			$(GO) run ./cmd/tidectl approve -stage-dir=.dev/migrations/tidectl/_staged -migrations-dir=.dev/migrations/tidectl; \
		fi; \
	fi
	@printf "\033[36m==> codegen\033[0m\n"
	@$(GO) run ./cmd/tidectl codegen
	@printf "\033[36m==> buf generate\033[0m\n"
	@buf generate >/dev/null
	@printf "\033[36m==> build\033[0m\n"
	@$(GO) build -o $(BIN_DIR)/atlantis ./cmd/server
	@printf "\033[32m==> ready\033[0m\n"

# dev-reset-db drops the local schema and re-applies all committed
# migrations from scratch. Useful when an iteration produced a bad
# migration that left the dev DB dirty, or when squashing accreted
# WIP migrations before opening a PR.
.PHONY: dev-reset-db
dev-reset-db: ## Drop local schema + reapply all committed migrations (DESTRUCTIVE)
	@echo "==> dropping schema 'atlantis' on $(PG_URL)..."
	@psql "$(PG_URL)" -c "DROP SCHEMA IF EXISTS atlantis CASCADE;" >/dev/null
	@psql "$(PG_URL)" -c "DROP TABLE IF EXISTS atlantis_schema_migrations_infra, atlantis_schema_migrations_tidectl CASCADE;" >/dev/null
	@echo "==> re-applying migrations..."
	@$(MAKE) migrate-up

# ---------- lint / quality ----------

.PHONY: lint
lint: ## Run static analysis (go vet + golangci-lint if installed)
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run; \
	else \
		echo "(golangci-lint not installed; skipping) install: brew install golangci-lint"; \
	fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

# ---------- CI gates ----------

# codegen-check: re-run codegen and fail if gen/, clients/go/, or atlantis/*.proto diverges. CI gate.
#
# `mkdir -p` on both sides of each diff handles the gen-less-repo case:
# on a fresh clone with no .atl fixtures the output dirs don't exist at
# all, and `diff -ruN` (which treats absent FILES as empty) still errors
# on a missing root directory. The mkdirs make both sides empty-but-
# present so an absent .atl fixture diffs cleanly.
.PHONY: codegen-check
codegen-check: ## Verify gen/ + clients/go/ + atlantis/*.proto are up to date with current .atl files
	@tmp=$$(mktemp -d) && \
	  $(GO) run ./cmd/tidectl codegen --out "$$tmp" --ir-checkpoint gen/.last-ir.json && \
	  mkdir -p gen "$$tmp/gen" clients/go/client "$$tmp/clients/go/client" \
	           atlantis/consumer "$$tmp/atlantis/consumer" \
	           atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" && \
	  diff -ruN gen "$$tmp/gen" >/dev/null && \
	  diff -ruN clients/go/client "$$tmp/clients/go/client" >/dev/null && \
	  diff -ruN atlantis/consumer "$$tmp/atlantis/consumer" >/dev/null && \
	  diff -ruN atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" >/dev/null && \
	  rm -rf "$$tmp" && echo "codegen-check ok" || \
	  (echo "codegen-check FAILED. Run 'make codegen' and commit the diff."; rm -rf "$$tmp"; exit 1)

# CI gate: up/down/up against fresh DB to catch broken .down.sql.
#
# Scoped to the infra history only. The tidectl history lives under
# .dev/migrations/tidectl/ which is gitignored — it's populated per-
# operator by `tidectl plan/approve` against their own .atl files and
# never lands in this repo, so CI can't roundtrip it.
.PHONY: migrate-roundtrip
migrate-roundtrip: ## Verify every infra migration is reversible against a fresh DB
	@which migrate >/dev/null || (echo "install golang-migrate: brew install golang-migrate" && exit 1)
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" down -all
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	@echo "migrate-roundtrip ok"

# ---------- clean ----------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
