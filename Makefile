# hydra CLI. `make help` lists the targets.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/Petatron/hydra/internal/cli.Version=$(VERSION)

LOCALBIN ?= $(shell pwd)/bin
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOVULNCHECK_VERSION ?= v1.9.0
ACTIONLINT_VERSION ?= v1.7.12

# Every OS/arch a release ships a binary for.
DIST_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

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

.PHONY: cilium-drift
cilium-drift: cilium ## Fail if the embedded Cilium manifest differs from the pinned chart
	git diff --exit-code internal/render/assets/cilium.yaml

.PHONY: dist
dist: ## Build release binaries for every DIST_PLATFORMS entry, with SHA256 checksums, into dist/
	@mkdir -p dist && rm -f dist/hydra-* dist/checksums.txt
	@for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building hydra-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/hydra-$$os-$$arch ./cmd/hydra || exit 1; \
	done
	@cd dist && shasum -a 256 hydra-* > checksums.txt && cat checksums.txt

# A release is a PR that changes VERSION (PET-92); see docs/releasing.md.
.PHONY: bump-patch bump-minor bump-major
bump-patch: ## Bump VERSION's patch number (fixes only)
	@hack/version.sh bump patch
bump-minor: ## Bump VERSION's minor number (new behaviour)
	@hack/version.sh bump minor
bump-major: ## Bump VERSION's major number
	@hack/version.sh bump major

.PHONY: check-version
check-version: ## Fail unless VERSION is well formed and not behind the newest tag (needs tags fetched)
	@hack/version.sh check

.PHONY: e2e
e2e: ## Bootstrap a kind management cluster and dry-run every example against it (needs Docker)
	./hack/e2e/run.sh

.PHONY: vuln
vuln: ## Check for known vulnerabilities in the code paths hydra uses
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: actionlint
actionlint: ## Lint the GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

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
