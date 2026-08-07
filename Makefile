CONFIG ?= config.yaml
BACKUP_DIR ?= backups
BACKUP_KEY_FILE ?= backup.key
BACKUP_NAME ?=
RESET_DATA_DIR ?= ./data
RESET_MASTER_KEY_FILE ?= ./master.key
# Optional sampling window for `make stats`. The underlying counters are
# cumulative since start, so an empty value reports a lifetime average and a
# duration such as 10s reports just that window — which is the one that means
# anything while an incident is in progress.
STATS_INTERVAL ?=

.DEFAULT_GOAL := help

# Release identity. The Dockerfile already injects these; a binary from `make
# build` reported "dev/unknown/unknown", so the version an operator read back
# from `heimdall version` depended on how it had been built. Overridable, so a
# release pipeline can pin all three rather than infer them from the checkout.
RELEASE_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# SOURCE_DATE_EPOCH, when set, keeps the stamp reproducible.
RELEASE_DATE ?= $(shell date -u -r "$${SOURCE_DATE_EPOCH:-$$(date +%s)}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO := github.com/akz142857/Heimdall/internal/buildinfo
GO_LDFLAGS := -X $(BUILDINFO).Version=$(RELEASE_VERSION) -X $(BUILDINFO).Commit=$(RELEASE_COMMIT) -X $(BUILDINFO).Date=$(RELEASE_DATE)

GO_SOURCES := $(shell find cmd internal -type f -name '*.go')
DEADMAN_SOURCES := $(shell find cmd/heimdall-deadman internal/deadman -type f -name '*.go')
WEBUI_DIST_SOURCES := $(shell find internal/webui/dist -type f)
WEB_SOURCES := $(shell find web/src web/scripts -type f) \
	web/index.html web/tsconfig.json web/tsconfig.app.json web/tsconfig.node.json web/vite.config.ts
WEB_DEPS_STAMP := web/node_modules/.heimdall-install-stamp
WEB_BUILD_STAMP := web/node_modules/.heimdall-build-stamp

.PHONY: help init-help setup init reset start dev build deadman frontend frontend-test backup stats test cover race vet fmt-check observability-check check clean version

help:
	@echo "Heimdall Makefile commands:"
	@echo "  help                 Show this help (default)"
	@echo "  init-help            Show detailed help for Heimdall init"
	@echo "  setup                Build Heimdall and dead-man"
	@echo "  init                 Initialize Heimdall using CONFIG"
	@echo "  reset                Reset data and Master Key (requires CONFIRM=RESET)"
	@echo "  start                Build and start Heimdall using CONFIG"
	@echo "  dev                  Build the frontend and start with go run"
	@echo "  build                Build Heimdall and dead-man"
	@echo "  deadman              Build only heimdall-deadman"
	@echo "  frontend             Install dependencies and build the frontend"
	@echo "  frontend-test        Run frontend tests"
	@echo "  backup               Create an encrypted backup"
	@echo "  stats                Show the durable write path of a running instance"
	@echo "  test                 Run all Go tests"
	@echo "  race                 Run all Go tests with the race detector"
	@echo "  vet                  Run go vet"
	@echo "  observability-check  Validate observability configuration"
	@echo "  check                Run test, race, vet, frontend-test, and observability-check"
	@echo "  clean                Remove binaries and frontend build stamps"
	@echo "  version              Show the release identity this build would carry"
	@echo ""
	@echo "Variables:"
	@echo "  CONFIG=$(CONFIG)"
	@echo "  BACKUP_DIR=$(BACKUP_DIR)"
	@echo "  BACKUP_KEY_FILE=$(BACKUP_KEY_FILE)"
	@echo "  BACKUP_NAME=$(BACKUP_NAME)"
	@echo "  RESET_DATA_DIR=$(RESET_DATA_DIR)"
	@echo "  RESET_MASTER_KEY_FILE=$(RESET_MASTER_KEY_FILE)"
	@echo "  STATS_INTERVAL=$(STATS_INTERVAL)"

init-help: bin/heimdall
	@./bin/heimdall init --help

setup: build

init: bin/heimdall
	./bin/heimdall init --config "$(CONFIG)"

reset:
	@confirm="$(CONFIRM)"; \
	echo "This permanently resets the Heimdall system."; \
	echo "Data directory: $(abspath $(RESET_DATA_DIR))"; \
	echo "Master Key:    $(abspath $(RESET_MASTER_KEY_FILE))"; \
	if [ -z "$$confirm" ] && [ -t 0 ]; then \
		printf "Type RESET to continue: "; \
		read -r confirm; \
	fi; \
	if [ "$$confirm" != "RESET" ]; then \
		echo "Reset cancelled."; \
		echo "For non-interactive use: make reset CONFIRM=RESET"; \
		exit 2; \
	fi
	@test "$(abspath $(RESET_DATA_DIR))" != "/" && test "$(abspath $(RESET_MASTER_KEY_FILE))" != "/"
	@test "$(abspath $(RESET_DATA_DIR))" != "$(abspath .)"
	@test ! -e "$(RESET_DATA_DIR)/.heimdall.lock" || ! command -v lsof >/dev/null || ! lsof "$(RESET_DATA_DIR)/.heimdall.lock" >/dev/null 2>&1 || \
		(echo "Refusing reset: Heimdall data directory is locked by a running process."; exit 1)
	rm -rf -- "$(RESET_DATA_DIR)"
	rm -f -- "$(RESET_MASTER_KEY_FILE)"
	$(MAKE) init CONFIG="$(CONFIG)"

start: build
	./bin/heimdall start --config "$(CONFIG)"

dev: frontend
	go run ./cmd/heimdall start --config "$(CONFIG)"

build: bin/heimdall bin/heimdall-deadman

deadman: bin/heimdall-deadman

backup: bin/heimdall
	./scripts/backup.sh \
		--binary "$(abspath bin/heimdall)" \
		--config "$(abspath $(CONFIG))" \
		--output-dir "$(abspath $(BACKUP_DIR))" \
		--key-file "$(abspath $(BACKUP_KEY_FILE))" $(if $(BACKUP_NAME),--name "$(BACKUP_NAME)")

# Reads a *running* instance over loopback, so unlike the offline targets this
# one needs Heimdall up and `metrics.enabled` true. It answers what bounds this
# instance's request rate without requiring a Prometheus install; the same
# summary is on Settings > Diagnostics.
stats: bin/heimdall
	./bin/heimdall stats --config "$(CONFIG)" $(if $(STATS_INTERVAL),--interval "$(STATS_INTERVAL)")

# The console bundle is committed under internal/webui/dist and embedded from
# there, so building the binary needs Go and nothing else — which is what the
# README promises. Rebuild the bundle explicitly with `make frontend` after
# changing anything under web/; CI fails on a stale one (`git diff --exit-code
# -- internal/webui/dist`), so it cannot drift unnoticed.
bin/heimdall: $(GO_SOURCES) $(WEBUI_DIST_SOURCES) go.mod go.sum
	mkdir -p bin
	go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $@ ./cmd/heimdall

bin/heimdall-deadman: $(DEADMAN_SOURCES) go.mod go.sum
	mkdir -p bin
	go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $@ ./cmd/heimdall-deadman

frontend: $(WEB_BUILD_STAMP)

$(WEB_DEPS_STAMP): web/package.json web/package-lock.json
	cd web && npm ci --ignore-scripts
	touch $@

$(WEB_BUILD_STAMP): $(WEB_DEPS_STAMP) $(WEB_SOURCES)
	cd web && npm run build
	touch $@

frontend-test: $(WEB_DEPS_STAMP)
	cd web && npm test

# -shuffle catches the order dependencies a fixed order hides: shared global
# state, one test leaving a clock or a directory another relies on. It found
# nothing on the day it was added, which is the point at which to add it —
# afterwards it stays cheap and only ever reports something new.
test:
	go test -shuffle=on ./...

# Coverage is not gated on a number. It exists so "which branch has no test"
# is a question the tooling answers rather than one somebody greps for by hand.
cover:
	go test -shuffle=on -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

race:
	go test -race ./...

vet:
	go vet ./...

observability-check:
	./deploy/observability/validate.sh

check: fmt-check test race vet frontend-test observability-check

# Nothing enforced gofmt, so an unformatted file reached main and stayed there.
# The convention in CLAUDE.md is only a convention until something fails on it.
fmt-check:
	@unformatted=$$(gofmt -l ./cmd ./internal ./tools); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

clean:
	rm -rf bin
	rm -f $(WEB_DEPS_STAMP) $(WEB_BUILD_STAMP) coverage.out

version:
	@echo "version: $(RELEASE_VERSION)"
	@echo "commit:  $(RELEASE_COMMIT)"
	@echo "date:    $(RELEASE_DATE)"
