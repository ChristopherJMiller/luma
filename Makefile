# luma - run every target inside the flake's shell:
#   nix develop -c make <target>
# The shell supplies go, node, pnpm, golangci-lint, helm, kind and
# postgresql; nothing here downloads tools.
#
# Targets for later tasks exist now as placeholders that print
# "not yet implemented (Task N)" and succeed, so `ci-ok` composes from the
# start. The task that implements a target replaces its recipe.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE := github.com/ChristopherJMiller/luma

# Version metadata, stamped in with -ldflags. DATE is the commit time, not
# "now", so two builds of one commit are identical.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X $(MODULE)/core/internal/version.Version=$(VERSION) \
	-X $(MODULE)/core/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/core/internal/version.Date=$(DATE)

GO_TEST_FLAGS ?= -race -count=1

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: nix develop -c make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: tools-versions
tools-versions: ## Print the version of every tool in the dev shell.
	@# `sed -n 1p` (not head) reads all input, so no tool dies of SIGPIPE.
	@go version
	@gopls version | sed -n 1p
	@golangci-lint version
	@govulncheck -version | sed -n '/^Scanner:/p'
	@echo "node $$(node --version)"
	@echo "pnpm $$(pnpm --version)"
	@postgres --version
	@kind version
	@kubectl version --client | sed -n 1p
	@echo "helm $$(helm version --short)"
	@docker --version
	@jq --version
	@yq --version
	@echo "actionlint $$(actionlint -version | sed -n 1p)"
	@echo "gitleaks $$(gitleaks version)"
	@openssl version
	@echo "libfaketime: $$LUMA_FAKETIME_LIB"
	@test -f "$$LUMA_FAKETIME_LIB"
	@echo "e2e kubeconfig: $$LUMA_E2E_KUBECONFIG"

##@ Development

.PHONY: fmt
fmt: ## Format Go sources.
	golangci-lint fmt ./...

.PHONY: vet
vet: ## go vet.
	go vet ./...

.PHONY: lint
lint: check-chart-sync ## golangci-lint, then helm lint over every chart CI values file.
	golangci-lint run ./...
	@# A no-op until Task 26a adds charts/luma/ci/*-values.yaml.
	@shopt -s nullglob; for f in charts/luma/ci/*-values.yaml; do \
		echo "helm lint --strict -f $$f"; \
		helm lint --strict charts/luma -f "$$f"; \
	done

##@ Test

.PHONY: test
test: test-go test-ts ## Go and TypeScript unit tests (never the spikes).

.PHONY: test-go
test-go: ## Go tests with the race detector.
	go test $(GO_TEST_FLAGS) ./...

.PHONY: test-ts
test-ts: ## TypeScript tests in every workspace package (spike files are excluded by their packages' configs).
	pnpm -r test

.PHONY: check-hygiene
check-hygiene: ## Fail on tracked binaries, big files, secret-type files or secrets in history.
	hack/check-hygiene.sh

.PHONY: check-versions
check-versions: ## Fail unless go.mod, the Dockerfile and the SDK/CLI pins agree with the flake.
	hack/check-versions.sh

.PHONY: check-chart-sync
check-chart-sync: ## Chart copies of agents/ equal the originals (Task 26a).
	@echo "check-chart-sync: not yet implemented (Task 26a)"

.PHONY: vulncheck
vulncheck: ## Known vulnerabilities in code Luma calls (govulncheck).
	govulncheck ./...

##@ Build

.PHONY: build
build: ## Build every Go package, version-stamped.
	go build -ldflags '$(LDFLAGS)' ./...

.PHONY: images
images: ## Build the core, conductor and smokescreen images.
	@echo "images: not yet implemented (Task 25)"

.PHONY: chart-test
chart-test: ## Render and assert on the Helm chart.
	@echo "chart-test: not yet implemented (Task 26a)"

##@ End to end

.PHONY: e2e
e2e: ## The kind suite.
	@echo "e2e: not yet implemented (Task 28a)"

.PHONY: e2e-up
e2e-up: ## Create the luma-e2e kind cluster (kubeconfig at .e2e/kubeconfig).
	@echo "e2e-up: not yet implemented (Task 28a)"

.PHONY: e2e-down
e2e-down: ## Delete the luma-e2e kind cluster.
	@echo "e2e-down: not yet implemented (Task 28a)"

##@ Spikes

.PHONY: gateway-spike
gateway-spike: ## Spike G: agentgateway passthrough.
	@echo "gateway-spike: not yet implemented (Task 3)"

.PHONY: sdk-spike
sdk-spike: ## Spike S: Agent SDK behaviour against the fake.
	@echo "sdk-spike: not yet implemented (Task 4)"

.PHONY: transcript-fixture
transcript-fixture: ## Regenerate the synthetic CLI transcript fixture.
	@echo "transcript-fixture: not yet implemented (Task 19b)"
