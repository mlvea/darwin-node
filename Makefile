ROOT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
export PATH := $(HOME)/.local/go/bin:$(PATH)

GO        ?= go
GOFLAGS   ?=
GOTEST    ?= $(GO) test
PKG       ?= ./...
BINDIR    ?= $(ROOT_DIR)bin
DISTDIR   ?= $(ROOT_DIR)dist
COVERFILE ?= $(ROOT_DIR)coverage.out
PYTHON    ?= python3

ENTITLEMENTS := $(ROOT_DIR)resources/entitlements/darwin-node.entitlements

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X github.com/darwin-node/darwin-node/pkg/config.Version=$(VERSION)

.PHONY: all build build-node build-agent build-image build-hwtest test test-short lint fmt vet tidy sign licenses dist clean help check test-hardware

all: fmt vet test build

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: build-node build-agent build-image build-hwtest ## Build all binaries


build-node: ## Build the host node agent
	@mkdir -p "$(BINDIR)"
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o "$(BINDIR)/darwin-node" ./cmd/darwin-node

build-agent: ## Build the in-guest agent
	@mkdir -p "$(BINDIR)"
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o "$(BINDIR)/darwin-guest-agent" ./cmd/guest-agent

build-image: ## Build the image bake/pack tool
	@mkdir -p "$(BINDIR)"
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o "$(BINDIR)/darwin-image" ./cmd/darwin-image

build-hwtest: ## Build the hardware release gate
	@mkdir -p "$(BINDIR)"
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o "$(BINDIR)/darwin-hwtest" ./cmd/darwin-hwtest

sign: build-node ## Ad-hoc codesign the host agent (and hw gate) with Virtualization entitlements
	codesign --force --sign - --entitlements "$(ENTITLEMENTS)" "$(BINDIR)/darwin-node"
	@if [ -f "$(BINDIR)/darwin-hwtest" ]; then \
		codesign --force --sign - --entitlements "$(ENTITLEMENTS)" "$(BINDIR)/darwin-hwtest"; \
	fi

# IMAGE=<baked image dir> make test-hardware
# Builds, signs, and runs the hardware gate. Skips (exit 0) when IMAGE is unset.
IMAGE ?=
test-hardware: build-hwtest sign ## Run the hardware release gate (needs a baked image)
	@if [ -z "$(IMAGE)" ]; then \
		echo "SKIP: set IMAGE=<baked image dir> to run the hardware gate"; \
	else \
		"$(BINDIR)/darwin-hwtest" --image "$(IMAGE)"; \
	fi

check: fmt vet test build ## Everything CI runs on every push

licenses: ## Regenerate THIRD_PARTY_NOTICES from the Go module graph
	$(PYTHON) "$(ROOT_DIR)hack/gen-third-party-notices.py"

dist: build ## Stage binaries plus LICENSE, NOTICE, THIRD_PARTY_NOTICES
	@test -s "$(ROOT_DIR)LICENSE" && test -s "$(ROOT_DIR)NOTICE" && test -s "$(ROOT_DIR)THIRD_PARTY_NOTICES" || \
		(echo "LICENSE, NOTICE, and THIRD_PARTY_NOTICES are required in dist; run make licenses" >&2; exit 1)
	rm -rf "$(DISTDIR)"
	mkdir -p "$(DISTDIR)"
	cp "$(BINDIR)/darwin-node" "$(BINDIR)/darwin-guest-agent" "$(BINDIR)/darwin-image" "$(BINDIR)/darwin-hwtest" "$(DISTDIR)/" 2>/dev/null || cp "$(BINDIR)/darwin-node" "$(BINDIR)/darwin-guest-agent" "$(BINDIR)/darwin-image" "$(DISTDIR)/"
	cp "$(ROOT_DIR)LICENSE" "$(ROOT_DIR)NOTICE" "$(ROOT_DIR)THIRD_PARTY_NOTICES" "$(DISTDIR)/"

test: ## Run all unit tests
	$(GOTEST) $(GOFLAGS) -count=1 -race -timeout 120s $(PKG)

test-short: ## Run tests that never need hardware
	$(GOTEST) $(GOFLAGS) -count=1 -short -timeout 60s $(PKG)

vet: ## go vet
	$(GO) vet $(PKG)

fmt: ## gofmt
	$(GO) fmt $(PKG)

tidy: ## go mod tidy
	$(GO) mod tidy

lint: ## golangci-lint if installed
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not installed; running go vet"; $(GO) vet $(PKG); fi

clean: ## Remove build artifacts
	rm -rf "$(BINDIR)" "$(DISTDIR)" "$(COVERFILE)"

coverage: ## Unit test coverage
	$(GOTEST) $(GOFLAGS) -coverprofile="$(COVERFILE)" -covermode=atomic $(PKG)
	$(GO) tool cover -func="$(COVERFILE)"
