# Local build, check, and test targets for Taletty.
# Follows ADR 003 (docs/adr/003-runtime-and-platform-matrix.md):
# - Independent Go modules (GOWORK=off GOTOOLCHAIN=local)
# - Isolated CPython 3.12 venv pinned to requirements.lock (never mutates system Python)
# - Never touches ~/.config/taletty/ or ~/.config/audible-tui/

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

GO ?= go
GO_ENV := GOWORK=off GOTOOLCHAIN=local
CGO_ENABLED ?= 1

BIN_DIR := $(CURDIR)/bin
COV_DIR := $(CURDIR)/coverage
ADAPTER_DIR := engine/connectors/audible/internal/bridge/adapter
LOCKFILE := $(ADAPTER_DIR)/requirements.lock
TOOLS_REQS := tools/requirements.txt

VENV ?= $(CURDIR)/.venv
HOST_PYTHON ?= python3.12
PYTHON ?= $(if $(wildcard $(VENV)/bin/python),$(abspath $(VENV)/bin/python),$(shell command -v $(HOST_PYTHON) 2>/dev/null || command -v python3 2>/dev/null || echo $(HOST_PYTHON)))

GO_TEST_FLAGS ?= -count=1 -race -p 1 -timeout=300s

.PHONY: help build build-engine build-app build-arm64 venv venv-test doctor check test coverage clean clean-venv

help:
	@printf '%s\n' \
		'Taletty local Makefile targets:' \
		'  make build         Build engine CLI (bin/taletty-engine) and app packages' \
		'  make build-engine  Build bin/taletty-engine from ./engine/cmd/taletty-engine' \
		'  make build-app     Build ./app packages (and bin/taletty once cmd/taletty exists)' \
		'  make build-arm64   Cross-build engine and app for linux/arm64 (compile check only)' \
		'  make venv          Create isolated CPython 3.12 venv at $(VENV) from requirements.lock' \
		'  make venv-test     Install test-only coverage tooling into $(VENV)' \
		'  make doctor        Run read-only bin/taletty-engine doctor --json with PYTHON=$(PYTHON)' \
		'  make check         Run inventory, gofmt, go mod tidy -diff, go vet, and Python compileall' \
		'  make test          Run tooling, schema, adapter, app, and engine test suites' \
		'  make coverage      Run full independent >=95% coverage gate into coverage/' \
		'  make clean         Remove bin/, coverage/, and Python __pycache__ artifacts' \
		'  make clean-venv    Remove $(VENV)' \
		'' \
		'Overrides: PYTHON=/path/to/venv/bin/python VENV=/path/to/venv GO_TEST_FLAGS="..."'

build: build-engine build-app

build-engine:
	@mkdir -p "$(BIN_DIR)"
	cd engine && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) build ./...
	cd engine && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) build -o "$(BIN_DIR)/taletty-engine" ./cmd/taletty-engine
	@printf 'Built %s\n' "$(BIN_DIR)/taletty-engine"

build-app:
	cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) build ./...
	@if [ -d app/cmd/taletty ]; then \
		mkdir -p "$(BIN_DIR)"; \
		cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) build -o "$(BIN_DIR)/taletty" ./cmd/taletty; \
		printf 'Built %s\n' "$(BIN_DIR)/taletty"; \
	else \
		printf 'Built app packages (app/cmd/taletty entrypoint not yet implemented; see #13)\n'; \
	fi

build-arm64:
	cd engine && $(GO_ENV) GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./...
	cd app && $(GO_ENV) GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./...
	@printf 'Cross-built engine and app for linux/arm64 (source compile check only; not runtime proof)\n'

venv:
	"$(HOST_PYTHON)" -m venv "$(VENV)"
	"$(VENV)/bin/python" -m pip install --no-deps -r "$(LOCKFILE)"
	@printf 'Prepared isolated runtime venv at %s\n' "$(VENV)"

venv-test: venv
	"$(VENV)/bin/python" -m pip install -r "$(TOOLS_REQS)"
	@printf 'Installed test-only tooling into %s\n' "$(VENV)"

doctor: build-engine
	TALETTY_PYTHON="$(PYTHON)" "$(BIN_DIR)/taletty-engine" doctor --json

check:
	"$(PYTHON)" tools/check_inventory.py --root .
	"$(PYTHON)" -m compileall -q tools tests/tooling "$(ADAPTER_DIR)" tests/adapter
	@for mod in app engine; do \
		( \
			set -euo pipefail; \
			cd "$$mod"; \
			test -z "$$(gofmt -l .)"; \
			$(GO_ENV) $(GO) mod tidy -diff; \
			$(GO_ENV) $(GO) vet ./...; \
		); \
	done

test:
	"$(PYTHON)" tools/check_inventory.py --root .
	"$(PYTHON)" -m unittest discover -s tests/tooling -v
	"$(PYTHON)" tests/storage/test_schema.py
	"$(PYTHON)" -B -m unittest discover -s tests/adapter -v
	@set -euo pipefail; \
	cache="$$(mktemp -d)"; \
	trap 'rm -rf -- "$$cache"' EXIT; \
	export TALETTY_SQLITE_TEST_CACHE="$$cache"; \
	(cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) test -count=1 -timeout=60s ./internal/store); \
	(cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) test $(GO_TEST_FLAGS) ./...)
	cd engine && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) TALETTY_TEST_PYTHON="$(PYTHON)" $(GO) test $(GO_TEST_FLAGS) ./...

coverage:
	@"$(PYTHON)" -c "import coverage" 2>/dev/null || { \
		printf 'Missing coverage package in %s; run "make venv-test" or pass PYTHON=/path/to/venv/bin/python\n' "$(PYTHON)" >&2; \
		exit 1; \
	}
	@mkdir -p "$(COV_DIR)"
	"$(PYTHON)" tools/check_inventory.py --root .
	"$(PYTHON)" -m compileall -q tools tests/tooling "$(ADAPTER_DIR)" tests/adapter
	COVERAGE_FILE="$(COV_DIR)/.coverage.tooling" "$(PYTHON)" -m coverage run --branch --source=tools -m unittest discover -s tests/tooling -v
	COVERAGE_FILE="$(COV_DIR)/.coverage.tooling" "$(PYTHON)" -m coverage report -m
	COVERAGE_FILE="$(COV_DIR)/.coverage.tooling" "$(PYTHON)" -m coverage json -o "$(COV_DIR)/tooling.json"
	"$(PYTHON)" tests/storage/test_schema.py
	COVERAGE_FILE="$(COV_DIR)/.coverage.adapter" "$(PYTHON)" -B -m coverage run --branch --source="$(ADAPTER_DIR)" -m unittest discover -s tests/adapter -v
	COVERAGE_FILE="$(COV_DIR)/.coverage.adapter" "$(PYTHON)" -m coverage report -m
	COVERAGE_FILE="$(COV_DIR)/.coverage.adapter" "$(PYTHON)" -m coverage json -o "$(COV_DIR)/adapter.json"
	"$(PYTHON)" tools/check_inventory.py --root . --adapter-report "$(COV_DIR)/adapter.json"
	@set -euo pipefail; \
	cache="$$(mktemp -d)"; \
	trap 'rm -rf -- "$$cache"' EXIT; \
	export TALETTY_SQLITE_TEST_CACHE="$$cache"; \
	(cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) test -count=1 -timeout=60s ./internal/store); \
	(cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) test $(GO_TEST_FLAGS) -covermode=atomic -coverpkg=./... -coverprofile="$(COV_DIR)/app.out" ./...); \
	(cd app && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) $(GO) test -count=3 -race -timeout=60s -run '^TestControllingTerminalJourney$$' -v ./internal/ui)
	cd engine && $(GO_ENV) CGO_ENABLED=$(CGO_ENABLED) TALETTY_TEST_PYTHON="$(PYTHON)" $(GO) test $(GO_TEST_FLAGS) -covermode=atomic -coverpkg=./... -coverprofile="$(COV_DIR)/engine.out" ./...
	"$(PYTHON)" tools/coverage_gate.py \
		--require tooling --require adapter --require app --require engine \
		--python tooling="$(COV_DIR)/tooling.json" \
		--python adapter="$(COV_DIR)/adapter.json" \
		--go app="$(COV_DIR)/app.out" \
		--go engine="$(COV_DIR)/engine.out"

clean:
	rm -rf -- "$(BIN_DIR)" "$(COV_DIR)" .coverage
	find tools tests engine -type d -name __pycache__ -prune -exec rm -rf -- {} +

clean-venv:
	rm -rf -- "$(VENV)"
