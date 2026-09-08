# debark-gui — developer convenience targets.
#
# Requires GNU Make and bash. This repository is developed on Windows as well
# as Linux — on Windows, run these from WSL or from Git Bash / MSYS2 with GNU
# make installed (`choco install make`, `scoop install make`). Every recipe is
# a plain go/gofmt/golangci-lint/wails invocation with no Make-specific
# behaviour, so if you have no `make` you can read the recipe and run it.
#
# Usage: make            (shows this help; also the default with no target)
#        make check      (what CI runs)
#        make build

SHELL       := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# Pinned tool versions. These live here and nowhere else: the workflows under
# .github/workflows/ read them back out of this file instead of repeating them,
# because a lint, a build or a release that behaves differently on a developer
# machine than in CI is worse than no check at all. The workflows parse these
# lines with sed rather than running `make`, because make is not guaranteed on
# the Windows runner image — so keep them as plain `NAME := vX.Y.Z`
# assignments, one per line, or the parsers fail loudly (which they are written
# to do). The engine pins the same golangci-lint,
# deliberately: a finding should not depend on which of the two modules you
# are standing in.
#
# Who reads which: ci.yml reads GOLANGCI_LINT_VERSION and WAILS_VERSION;
# release.yml reads GORELEASER_VERSION - both of its jobs do, the CLI's and
# this application's, out of this one line; licence-scan.yml reads
# GO_LICENSES_VERSION.
#
# GORELEASER_VERSION is an exact version rather than the `~> v2` range the core
# repository's release workflow uses, for the same reason the actions are pinned
# by SHA: a release produced by a different goreleaser than the one anybody
# dry-ran with is a release nobody rehearsed.
#
# GO_LICENSES_VERSION is pinned where the core repository installs `@latest`.
# The tool that decides whether this project's licensing is acceptable must not
# change under it between two runs with no diff anywhere.
GOLANGCI_LINT_VERSION := v2.13.2
WAILS_VERSION         := v2.12.0
GORELEASER_VERSION    := v2.18.0
GO_LICENSES_VERSION   := v2.0.1

# The engine dependency.
#
# go.mod carries `replace github.com/inferops/debark => ../` because the engine
# is not published to a module proxy. It does not need to be: the engine lives
# in this same repository, one directory up, so any checkout builds the GUI
# against exactly the engine it ships with. There is no commit to pin and no
# second checkout to keep in step - which is why the two live together.
#
# This is permanent for this layout, not a staging post: two Go modules in one
# repository resolve each other by path. Publishing the engine to a module proxy
# and turning the directive into a `require` would mean splitting the repository
# back into two, which is not the plan. docs/release.md, section "The replace
# directive", is the long version.

# Wails links against webkit2gtk on Linux, and picks the ABI with a build tag:
# no tag means webkit2gtk-4.0, `webkit2_41` means webkit2gtk-4.1. Debian 13,
# Ubuntu 24.04 and everything newer ship only 4.1, so 4.1 is the default here
# and 4.0 is the exception:
#
#     make build WEBKIT_TAG=          # webkit2gtk-4.0
#
# This comment used to name Ubuntu 22.04 and Debian 12 as the releases needing
# that exception. They do not: both ship 4.1 as well as 4.0, measured rather
# than assumed — bookworm offers libwebkit2gtk-4.1-0 and libwebkit2gtk-4.0-37
# at the same source version (2.50.6-1~deb12u2), and jammy/universe has 4.1 at
# 2.36.0-2ubuntu1. So the default tag builds on them too, and there is no
# release this project targets where clearing WEBKIT_TAG is required. It is
# kept for a host that genuinely has only 4.0 headers installed.
#
# Neither release can install the .deb regardless, and not because of WebKit:
# they are excluded by the t64 transition, so the install fails cleanly on
# libglib2.0-0t64/libgtk-3-0t64. See docs/packaging.md.
#
# The tag is passed on every Go command, not just `wails build`, because the
# cgo directives are in packages that `go build ./...`, `go vet` and `go test`
# all have to compile. On Windows and macOS it matches nothing and is inert,
# which is why it is unconditional rather than guarded by an OS check.
WEBKIT_TAG ?= webkit2_41
GOFLAGS_TAGS := $(if $(WEBKIT_TAG),-tags $(WEBKIT_TAG),)

# The tags a SHIPPING build needs, as one comma-separated list for GOFLAGS.
# `desktop,production` are mandatory and are the difference between a binary
# that runs and one that compiles and then refuses to start, printing "Wails
# applications will not build without the correct build tags". The targets
# above do not need them because `wails build` adds them itself; anything that
# invokes the Go toolchain directly on a shipping configuration does — see
# .goreleaser.yaml, hack/reproducible-check.sh and the licences target below.
RELEASE_TAGS := desktop,production
ifneq ($(WEBKIT_TAG),)
RELEASE_TAGS := $(RELEASE_TAGS),$(WEBKIT_TAG)
endif

.DEFAULT_GOAL := help

.PHONY: help build dev frontend test lint fmt fmt-check vet tidy clean check \
        snapshot release-check repro licences \
        print-golangci-lint-version print-wails-version \
        print-goreleaser-version print-go-licenses-version

help: ## Show this help
	@echo "debark-gui - make targets:"
	@echo
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-11s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the desktop app into build/bin (runs the frontend copy step itself, via wails.json)
	wails build $(GOFLAGS_TAGS)

dev: ## Run the app with live asset reloading (wails dev; serves frontend/dist from disk)
	wails dev $(GOFLAGS_TAGS)

frontend: ## Rebuild frontend/dist — a plain copy of index.html, src/ and wailsjs/. No npm, no bundler.
	go run ./hack/copyfrontend

test: ## Run the Go tests
	go test $(GOFLAGS_TAGS) -count=1 ./...

vet: ## Run go vet
	go vet $(GOFLAGS_TAGS) ./...

lint: ## Run golangci-lint (.golangci.yml); installs nothing, tells you if it is missing
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found on PATH."; \
		echo "Install the pinned version:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"; \
		exit 1; \
	}
	@have="$$(golangci-lint version --short 2>/dev/null || true)"; \
	want="$(GOLANGCI_LINT_VERSION)"; \
	if [ -n "$$have" ] && [ "$$have" != "$${want#v}" ]; then \
		echo "warning: golangci-lint $$have is on PATH; this repository pins $$want."; \
		echo "         Findings below may not match CI."; \
		echo; \
	fi
	golangci-lint run ./...

fmt: ## Reformat all Go source with gofmt
	gofmt -l -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted (does not rewrite anything)
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt-formatted:"; \
		echo "$$unformatted"; \
		echo; \
		echo "Run 'make fmt' and commit the result."; \
		exit 1; \
	fi; \
	echo "All Go files are gofmt-formatted."

tidy: ## Tidy go.mod/go.sum and report whether that changed anything
	go mod tidy
	@if ! git diff --quiet -- go.mod go.sum; then \
		echo "go.mod/go.sum changed. Review and commit the result:"; \
		git --no-pager diff --stat -- go.mod go.sum; \
	fi

clean: ## Remove build output and empty frontend/dist (keeping its .gitkeep)
	rm -rf build/bin dist debark-gui debark-gui.exe coverage.out
	go run ./hack/copyfrontend -clean

# ---------------------------------------------------------------------------
# Release. .goreleaser.yaml is the configuration and docs/release.md explains
# what a release actually does. Read that before cutting one — in particular
# the sections on what reproducibility means here (this application links GTK3
# and WebKitGTK through cgo, so the debark claim does not transfer unchanged)
# and on the `replace` directive.
# ---------------------------------------------------------------------------

release-check: ## Validate .goreleaser.yaml. Builds nothing.
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser not found on PATH."; \
		echo "Install the pinned version:"; \
		echo "  go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)"; \
		echo; \
		echo "Note: that go install pulls a NEWER Go toolchain than this project"; \
		echo "uses — goreleaser $(GORELEASER_VERSION) declares go >= 1.27.0 — so it"; \
		echo "fails outright under GOTOOLCHAIN=local. CI never hits this, because"; \
		echo "goreleaser-action downloads a release binary. docs/release.md §8."; \
		exit 1; \
	}
	goreleaser check

snapshot: ## Local dry run of the whole release. Publishes nothing, needs no tag.
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser not found on PATH."; \
		echo "Install the pinned version:"; \
		echo "  go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)"; \
		echo; \
		echo "Note: that go install pulls a NEWER Go toolchain than this project"; \
		echo "uses — goreleaser $(GORELEASER_VERSION) declares go >= 1.27.0 — so it"; \
		echo "fails outright under GOTOOLCHAIN=local. CI never hits this, because"; \
		echo "goreleaser-action downloads a release binary. docs/release.md §8."; \
		exit 1; \
	}
	goreleaser release --snapshot --clean

repro: ## Build twice with the release flags and prove the outputs are byte-identical
	bash hack/reproducible-check.sh

# What .github/workflows/licence-scan.yml runs. The shipping build tags are not
# optional here: Wails selects its whole frontend implementation by build tag,
# so without them this scans a different set of packages from the one that links.
licences: ## Report dependency licences; fail on copyleft (go-licenses)
	@command -v go-licenses >/dev/null 2>&1 || { \
		echo "go-licenses not found on PATH."; \
		echo "Install the pinned version:"; \
		echo "  go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)"; \
		exit 1; \
	}
	GOFLAGS="-tags=$(RELEASE_TAGS)" go-licenses report ./...
	GOFLAGS="-tags=$(RELEASE_TAGS)" go-licenses check ./... \
		--disallowed_types=forbidden,restricted

# check is what .github/workflows/ci.yml runs, in this order, so that a red CI
# can be reproduced with one command. Formatting first because a gofmt failure
# is the cheapest to diagnose and would otherwise be buried under lint output;
# vet before golangci-lint because vet failures are usually real bugs; tests
# last because they are the slowest.
check: fmt-check vet lint test ## Everything CI runs: fmt-check + vet + lint + test

# Plumbing: the pinned versions, for scripts and for answering "what does CI
# install?" without reading the workflow. Deliberately undocumented in
# `make help` — not targets anyone runs by hand.
print-golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

print-wails-version:
	@echo $(WAILS_VERSION)

print-goreleaser-version:
	@echo $(GORELEASER_VERSION)

print-go-licenses-version:
	@echo $(GO_LICENSES_VERSION)
