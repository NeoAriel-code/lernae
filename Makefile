SHELL := /bin/sh

GO ?= go
NPM ?= npm
PYTHON ?= python3
GO_FILES := $(shell find cmd internal -type f -name '*.go')
GO_PACKAGES := ./cmd/... ./internal/...

.PHONY: help setup dev build test lint typecheck

help: ## Show available development commands
	@printf '%s\n' \
	  'Lernae Foundation commands:' \
	  '  make setup      Check prerequisites and install locked Web dependencies' \
	  '  make dev        Run the static Web scaffold on loopback' \
	  '  make build      Build Go scaffolds and the Web application' \
	  '  make test       Run Go tests and JSON syntax checks' \
	  '  make lint       Check Go formatting, run go vet, and lint Web sources' \
	  '  make typecheck  Type-check the Web application'

setup: ## Check prerequisites and install locked Web dependencies
	@command -v $(GO) >/dev/null || { echo 'Go is required (see go.mod).'; exit 1; }
	@command -v node >/dev/null || { echo 'Node.js 22.12+ is required by Vite.'; exit 1; }
	@node -e 'const [major, minor] = process.versions.node.split(".").map(Number); if (major < 22 || (major === 22 && minor < 12)) { console.error("Node.js 22.12+ is required by Vite."); process.exit(1); }'
	@command -v $(NPM) >/dev/null || { echo 'npm is required.'; exit 1; }
	@command -v $(PYTHON) >/dev/null || { echo 'Python 3 is required for JSON syntax checks.'; exit 1; }
	$(NPM) --prefix apps/web ci

dev: ## Run the static Web scaffold on loopback
	$(NPM) --prefix apps/web run dev -- --host 127.0.0.1

build: ## Build Go scaffolds and the Web application
	$(GO) build $(GO_PACKAGES)
	$(NPM) --prefix apps/web run build

test: ## Run Go tests and JSON syntax checks
	$(GO) test $(GO_PACKAGES)
	$(PYTHON) -m json.tool schemas/inventory-manifest.schema.json >/dev/null
	$(PYTHON) -m json.tool examples/manifest.json >/dev/null

lint: ## Check Go formatting, run go vet, and lint Web sources
	@test -z "$(shell gofmt -l $(GO_FILES))" || { echo 'Go files need gofmt.'; gofmt -l $(GO_FILES); exit 1; }
	$(GO) vet $(GO_PACKAGES)
	$(NPM) --prefix apps/web run lint

typecheck: ## Type-check the Web application
	$(NPM) --prefix apps/web run typecheck
