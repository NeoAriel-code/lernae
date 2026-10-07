SHELL := /bin/bash

GO ?= go
NPM ?= npm
GO_PACKAGES := ./cmd/... ./internal/... ./migrations ./schemas
GO_FILES := $(shell find cmd internal migrations schemas -type f -name '*.go')
BUILD_DIR := build

.PHONY: help setup dev server agent web configure configure-local configure-igdb configure-romm configure-prowlarr configure-qbittorrent configure-status build test lint typecheck

help: ## Show available development commands
	@printf '%s\n' \
	  'Lernae runtime commands:' \
	  '  make setup      Check prerequisites and install locked Go and Web dependencies' \
	  '  make dev        Start Agent, Server, and Web together for local development' \
	  '  make server     Start only the Go Server' \
	  '  make agent      Start only the local Go Agent' \
	  '  make web        Start only the Vite Web development server' \
	  '  make configure  Configure saved runtime settings and optionally providers' \
	  '  make configure-local  Configure Server-local acquisition and private existing roots' \
	  '  make configure-igdb  Configure IGDB credentials through hidden terminal input' \
	  '  make configure-romm  Configure RomM credentials through hidden terminal input' \
	  '  make configure-prowlarr  Configure optional Prowlarr discovery and hidden API key' \
	  '  make configure-qbittorrent  Configure optional qBittorrent execution and hidden password' \
	  '  make configure-status  Show saved settings and provider booleans (no secrets)' \
	  '  make lint       Check Go formatting, run go vet, and lint Web sources' \
	  '  make typecheck  Type-check the Web application' \
	  '  make test       Run deterministic Go tests, including SQLite, UDS, and manifest checks' \
	  '  make build      Build both Go binaries and the Web production bundle'

setup: ## Check prerequisites and install Go and locked Web dependencies
	@command -v $(GO) >/dev/null || { echo 'Go 1.27+ is required (see go.mod).'; exit 1; }
	@version="$$($(GO) env GOVERSION)"; version="$${version#go}"; major="$${version%%.*}"; rest="$${version#*.}"; minor="$${rest%%.*}"; \
	  if (( major < 1 || (major == 1 && minor < 27) )); then echo "Go 1.27+ is required; found $$($(GO) version)."; exit 1; fi
	@command -v node >/dev/null || { echo 'Node.js 22.12+ is required by Vite.'; exit 1; }
	@node -e 'const [major, minor] = process.versions.node.split(".").map(Number); if (major < 22 || (major === 22 && minor < 12)) { console.error("Node.js 22.12+ is required by Vite."); process.exit(1); }'
	@command -v $(NPM) >/dev/null || { echo 'npm is required.'; exit 1; }
	$(GO) mod download
	$(NPM) --prefix apps/web ci

dev: ## Start Agent, Server, and Web together for local development
	GO="$(GO)" ./scripts/dev.sh

server: ## Start only the Go Server
	$(GO) run ./cmd/server

agent: ## Start only the local Go Agent
	$(GO) run ./cmd/agent

web: ## Start only the Vite Web development server
	LERNAE_DEV_API_PROXY_TARGET="$$($(GO) run ./cmd/provider-config dev-api-proxy-target)" $(NPM) --prefix apps/web run dev -- --host 127.0.0.1

configure: ## Configure saved runtime settings and optional providers
	$(GO) run ./cmd/provider-config configure

configure-local: ## Configure persistent Server-local acquisition (disabled by default)
	$(GO) run ./cmd/provider-config configure local

configure-igdb: ## Configure IGDB credentials through hidden terminal input
	$(GO) run ./cmd/provider-config configure igdb

configure-romm: ## Configure RomM credentials through hidden terminal input
	$(GO) run ./cmd/provider-config configure romm

configure-prowlarr: ## Configure persistent Prowlarr Discovery only (disabled by default)
	$(GO) run ./cmd/provider-config configure prowlarr

configure-qbittorrent: ## Configure persistent qBittorrent execution (disabled by default)
	$(GO) run ./cmd/provider-config configure qbittorrent

configure-status: ## Show saved settings and provider booleans without secrets
	$(GO) run ./cmd/provider-config status

build: ## Build both Go binaries and the Web production bundle
	mkdir -p $(BUILD_DIR)
	$(GO) build -o $(BUILD_DIR)/lernae-server ./cmd/server
	$(GO) build -o $(BUILD_DIR)/lernae-agent ./cmd/agent
	$(NPM) --prefix apps/web run build

test: ## Run deterministic Go tests, including SQLite, UDS, and manifest checks
	$(GO) test $(GO_PACKAGES)

lint: ## Check Go formatting, run go vet, and lint Web sources
	@test -z "$(shell gofmt -l $(GO_FILES))" || { echo 'Go files need gofmt.'; gofmt -l $(GO_FILES); exit 1; }
	$(GO) vet $(GO_PACKAGES)
	$(NPM) --prefix apps/web run lint

typecheck: ## Type-check the Web application
	$(NPM) --prefix apps/web run typecheck
