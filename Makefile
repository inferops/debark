# debark — developer convenience targets.
#
# Requires GNU Make. This repository is developed on Windows as well as
# Linux/macOS, and `make` itself is not a standard Windows tool — if you are
# on Windows, either:
#   - run these from WSL, or from Git Bash / MSYS2 with GNU make installed
#     (e.g. `choco install make`, `scoop install make`), or
#   - skip Make entirely and run the command each target wraps directly; every
#     recipe below is a plain go/gofmt/golangci-lint/goreleaser invocation
#     with no Make-specific behaviour, and is shown in full so you can copy
#     it. hack/linux-test.sh itself only needs bash + Docker and already
#     runs fine from Git Bash with no `make` involved.
#
# Usage: make            (shows this help; also the default with no target)
#        make build
#        make test-linux PKGS=./core/apt/...

SHELL       := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

MODULE    := github.com/inferops/debark
BIN       := debark
DIST      := dist
COVERFILE := coverage.out
PKGS      ?=

# The one place the golangci-lint version is written down. .github/workflows/
# ci.yml reads it from here (`make -s print-golangci-lint-version`) instead of
# repeating it, because a lint config is only useful if the run a developer
# does locally is the run CI does - and the two had drifted: CI pinned
# v2.12.2 while developers were on v2.13.2, so a finding could differ by tool
# version and nobody could tell that apart from a real regression.
#
# Bumping this is a deliberate act: a new golangci-lint ships new and changed
# analysers, so expect the finding set to move. Bump it here, run `make lint`,
# and land the resulting fixes in the same change.
GOLANGCI_LINT_VERSION := v2.13.2
GOVULNCHECK_VERSION := v1.1.4

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Edition is deliberately left at its build-time default ("community") for
# local builds — only .goreleaser.yaml's release pipeline sets
# Edition=official, because only that pipeline is this project's own
# release process (TRADEMARK.md, ADR-010, core/version/version.go).
LDFLAGS := -s -w \
	-X '$(MODULE)/core/version.Version=$(VERSION)' \
	-X '$(MODULE)/core/version.Commit=$(COMMIT)' \
	-X '$(MODULE)/core/version.Date=$(DATE)'

.DEFAULT_GOAL := help

.PHONY: help build test test-linux lint fmt vet cover man completions snapshot clean print-golangci-lint-version

help: ## Show this help
	@echo "debark - make targets:"
	@echo
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-13s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build ./cmd/debark (needs internal/cli + cmd/debark implemented)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/debark

test: ## Run unit tests (portable; skips anything needing a real apt)
	go test -count=1 ./...

test-linux: ## Run tests inside the Debian 12 container via hack/linux-test.sh (set DEBARK_E2E=1 for apt-backed tests, PKGS to scope)
	bash hack/linux-test.sh $(PKGS)

lint: ## Run golangci-lint (.golangci.yml); installs nothing, tells you if it is missing
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found on PATH."; \
		echo "Install the pinned version:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"; \
		echo "Other install methods: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	@have="$$(golangci-lint version --short 2>/dev/null || true)"; \
	want="$(GOLANGCI_LINT_VERSION)"; \
	if [ -n "$$have" ] && [ "$$have" != "$${want#v}" ]; then \
		echo "warning: golangci-lint $$have is on PATH; this repository pins $$want."; \
		echo "         Findings below may not match CI. To match CI exactly:"; \
		echo "         go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$$want"; \
		echo; \
	fi
	golangci-lint run ./...

# Read by .github/workflows/ci.yml so the pinned version lives in exactly one
# file. Deliberately not documented in `make help`: it is plumbing, not a
# target anyone runs by hand.
print-golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

.PHONY: print-govulncheck-version vulncheck
print-govulncheck-version:
	@echo $(GOVULNCHECK_VERSION)

vulncheck: ## Check CLI/engine dependencies against the Go vulnerability database
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fmt: ## Reformat all Go source with gofmt
	gofmt -l -w .

vet: ## Run go vet
	go vet ./...

cover: ## Run tests with coverage; prints a summary and how to view the HTML report
	go test -count=1 -coverprofile=$(COVERFILE) ./...
	go tool cover -func=$(COVERFILE) | tail -1
	@echo "HTML report: go tool cover -html=$(COVERFILE)"

man: ## Generate man pages into ./man (uses cmd/debark's hidden `gen-manpages` command — see .goreleaser.yaml's before-hook)
	@mkdir -p man
	go run ./cmd/debark gen-manpages ./man

completions: ## Generate shell completions into ./completions (needs cmd/debark's `completion` command)
	@mkdir -p completions
	@for sh in bash zsh fish powershell; do \
		go run ./cmd/debark completion $$sh > completions/debark.$$sh ; \
	done

snapshot: ## Local goreleaser dry run: build every platform, skip publish/sign/announce
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser not found on PATH."; \
		echo "Install: https://goreleaser.com/install/"; \
		exit 1; \
	}
	goreleaser release --snapshot --clean --skip=publish,sign,announce

clean: ## Remove build artefacts (binary, dist/, coverage, generated docs)
	rm -rf $(BIN) $(BIN).exe $(DIST) $(COVERFILE) man completions
