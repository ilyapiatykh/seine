SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE       ?= github.com/ilyapiatykh/seine
BIN_DIR      ?= bin
LDFLAGS_PKG  := $(MODULE)/internal/buildinfo

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(LDFLAGS_PKG).Version=$(VERSION) \
	-X $(LDFLAGS_PKG).Commit=$(COMMIT) \
	-X $(LDFLAGS_PKG).BuildDate=$(BUILD_DATE)

GO_BUILD_FLAGS  ?= -trimpath -ldflags "$(LDFLAGS)"
GO_TEST_FLAGS   ?= -race -count=1
GO_VET_TARGETS  ?= ./...

BINARIES := seine-server seine-agent

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: $(addprefix $(BIN_DIR)/,$(BINARIES)) ## Build all binaries into $(BIN_DIR).

$(BIN_DIR)/%: cmd/%
	@mkdir -p $(BIN_DIR)
	go build $(GO_BUILD_FLAGS) -o $@ ./$<

.PHONY: install
install: ## go install all binaries (uses GOBIN).
	go install $(GO_BUILD_FLAGS) ./cmd/...

.PHONY: test
test: ## Run unit tests with race detector.
	go test $(GO_TEST_FLAGS) ./...

.PHONY: vet
vet: ## go vet.
	go vet $(GO_VET_TARGETS)

.PHONY: fmt
fmt: ## Format Go sources with gofmt.
	gofmt -w -s $(shell git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './vendor/*')

.PHONY: fmt-check
fmt-check: ## Check that sources are gofmt-clean (CI).
	@out=$$(gofmt -l -s $(shell git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './vendor/*')); \
	if [ -n "$$out" ]; then echo "gofmt diff in:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## golangci-lint (if installed).
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping"; \
	fi

.PHONY: tidy
tidy: ## go mod tidy.
	go mod tidy

.PHONY: proto
proto: ## Regenerate gRPC code from .proto files.
	@command -v protoc >/dev/null || { echo "protoc is not installed"; exit 1; }
	@command -v protoc-gen-go >/dev/null || { echo "install protoc-gen-go: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null || { echo "install protoc-gen-go-grpc: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"; exit 1; }
	protoc \
		--proto_path=api/proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(shell find api/proto -name '*.proto')

COMPOSE_FILE := deploy/compose/docker-compose.yml
COMPOSE      := docker compose -f $(COMPOSE_FILE)

.PHONY: demo-up
demo-up: ## Start the docker-compose demo (build + up -d).
	$(COMPOSE) up --build -d

.PHONY: demo-down
demo-down: ## Stop the demo (preserves volumes).
	$(COMPOSE) down

.PHONY: demo-clean
demo-clean: ## Stop the demo and delete volumes.
	$(COMPOSE) down -v

.PHONY: demo-status
demo-status: ## docker compose ps for the demo.
	$(COMPOSE) ps

.PHONY: demo-logs
demo-logs: ## Follow logs of all demo services.
	$(COMPOSE) logs -f

.PHONY: demo-verify
demo-verify: ## Ping spoke-office from spoke-cloud through the overlay.
	$(COMPOSE) exec -T spoke-cloud ping -c 3 100.64.2.10

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) dist
