CONFIG ?= config.yaml
BACKUP_DIR ?= backups
BACKUP_KEY_FILE ?= backup.key
BACKUP_NAME ?=

GO_SOURCES := $(shell find cmd internal -type f -name '*.go')
DEADMAN_SOURCES := $(shell find cmd/heimdall-deadman internal/deadman -type f -name '*.go')
WEB_SOURCES := $(shell find web/src web/scripts -type f) \
	web/index.html web/tsconfig.json web/tsconfig.app.json web/tsconfig.node.json web/vite.config.ts
WEB_DEPS_STAMP := web/node_modules/.heimdall-install-stamp
WEB_BUILD_STAMP := web/node_modules/.heimdall-build-stamp

.PHONY: setup init start dev build deadman frontend frontend-test backup test race vet observability-check check clean

setup: build

init: bin/heimdall
	./bin/heimdall init --config "$(CONFIG)"

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

bin/heimdall: $(GO_SOURCES) go.mod go.sum $(WEB_BUILD_STAMP)
	mkdir -p bin
	go build -trimpath -o $@ ./cmd/heimdall

bin/heimdall-deadman: $(DEADMAN_SOURCES) go.mod go.sum
	mkdir -p bin
	go build -trimpath -o $@ ./cmd/heimdall-deadman

frontend: $(WEB_BUILD_STAMP)

$(WEB_DEPS_STAMP): web/package.json web/package-lock.json
	cd web && npm ci --ignore-scripts
	touch $@

$(WEB_BUILD_STAMP): $(WEB_DEPS_STAMP) $(WEB_SOURCES)
	cd web && npm run build
	touch $@

frontend-test: $(WEB_DEPS_STAMP)
	cd web && npm test

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

observability-check:
	./deploy/observability/validate.sh

check: test race vet frontend-test observability-check

clean:
	rm -rf bin
	rm -f $(WEB_DEPS_STAMP) $(WEB_BUILD_STAMP)
