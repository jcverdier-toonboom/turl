BIN_DIR   := bin
BINARY    := $(BIN_DIR)/turl
PKG       := ./...
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOBIN_DIR := $(shell go env GOPATH)/bin

STATICCHECK   := $(shell command -v staticcheck 2>/dev/null || echo $(GOBIN_DIR)/staticcheck)
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null)

.DEFAULT_GOAL := build
.PHONY: help build build-all run test test-race cover lint fmt vet staticcheck tidy install clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build bin/turl for this machine
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

build-all: ## Cross-compile into bin/ for macOS and Linux (amd64, arm64)
	@mkdir -p $(BIN_DIR)
	@for os in darwin linux; do for arch in amd64 arm64; do \
		echo "→ $(BIN_DIR)/turl-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/turl-$$os-$$arch . || exit 1; \
	done; done

run: build ## Build and open the TUI (pass ARGS="--dir ...")
	$(BINARY) $(ARGS)

test: ## Run unit tests
	go test $(PKG)

test-race: ## Run unit tests with the race detector
	go test -race -count=1 $(PKG)

cover: ## Run tests with coverage, write bin/coverage.html
	@mkdir -p $(BIN_DIR)
	go test -coverprofile=$(BIN_DIR)/coverage.out $(PKG)
	go tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html
	@go tool cover -func=$(BIN_DIR)/coverage.out | tail -1

lint: vet staticcheck ## Check formatting, go vet, staticcheck (and golangci-lint if installed)
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
ifneq ($(GOLANGCI_LINT),)
	$(GOLANGCI_LINT) run
endif

fmt: ## Format the code
	gofmt -w .

vet:
	go vet $(PKG)

staticcheck:
	@test -x "$(STATICCHECK)" || go install honnef.co/go/tools/cmd/staticcheck@latest
	$(STATICCHECK) $(PKG)

tidy: ## go mod tidy
	go mod tidy

install: ## Install turl into $GOPATH/bin
	go install -trimpath -ldflags '$(LDFLAGS)' .

clean: ## Remove bin/
	rm -rf $(BIN_DIR)
