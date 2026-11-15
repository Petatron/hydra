# hydra CLI. `make help` lists the targets.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/Petatron/hydra/internal/cli.Version=$(VERSION)

LOCALBIN ?= $(shell pwd)/bin
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build bin/hydra
	go build -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/hydra ./cmd/hydra

.PHONY: test
test: ## Run unit and golden-file tests
	go test ./... -coverprofile cover.out

.PHONY: golden
golden: ## Rewrite render golden files after an intended output change (review the diff!)
	go test ./internal/render -update

.PHONY: cilium
cilium: ## Regenerate the embedded Cilium manifest (needs helm)
	./hack/gen-cilium.sh

.PHONY: verify
verify: ## Fail if go.mod is untidy or gofmt would change anything
	go mod tidy -diff
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	"$(GOLANGCI_LINT)" run

.PHONY: lint-config
lint-config: $(GOLANGCI_LINT) ## Verify the golangci-lint configuration
	"$(GOLANGCI_LINT)" config verify

$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
