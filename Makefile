# monokasa — local build/run without docker.
#
# Quick reference:
#   make            — show this help
#   make build      — build Svelte SPA → embed into Go binary
#   make run        — build, then ./monokasa (loads .env via godotenv)
#   make test       — go test ./...
#   make clean      — remove binary, frontend node_modules and build dirs
#
# Docker remains the recommended path (one container, all batteries
# included). This Makefile is for the case where you want a native
# binary on the host — same artefact, no container overhead.

GO            ?= go
NPM           ?= npm
BIN           := monokasa
FRONTEND_DIR  := frontend
EMBED_DIR     := internal/webui/dist

.DEFAULT_GOAL := help

.PHONY: help build frontend backend run test deps clean clean-dist

help: ## Show available targets
	@printf "monokasa — local build targets\n\n"
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: frontend backend ## Build Svelte + embed + Go binary (full chain)

frontend: ## Build Svelte SPA and replace the embed dir with the result
	cd $(FRONTEND_DIR) && $(NPM) install && $(NPM) run build
	rm -rf $(EMBED_DIR)
	cp -r $(FRONTEND_DIR)/build $(EMBED_DIR)

backend: ## Build Go binary (assumes $(EMBED_DIR) is already populated)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/app

run: build ## Build everything, then run the binary
	./$(BIN)

test: ## Run the full Go test suite
	$(GO) test ./...

deps: ## Download Go module dependencies
	$(GO) mod download

clean: ## Remove binary and frontend build artefacts
	rm -f $(BIN)
	rm -rf $(FRONTEND_DIR)/build
	rm -rf $(FRONTEND_DIR)/node_modules
	rm -rf $(FRONTEND_DIR)/.svelte-kit

clean-dist: ## Restore the embed-dir stub (run after `make frontend` overwrote it)
	git checkout -- $(EMBED_DIR)
