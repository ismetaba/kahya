SQLC_VERSION ?= v1.30.0        # pin; bump deliberately, never 'latest'
VENV := worker/.venv
PY := $(VENV)/bin/python

CODESIGN_ID ?= Kahya Dev

KAHYA_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
KAHYA_DATA_DIR := $(HOME)/Library/Application Support/Kahya
KAHYA_LOG_DIR := $(KAHYA_DATA_DIR)/logs
LAUNCH_AGENTS_DIR := $(HOME)/Library/LaunchAgents
PLIST_NAME := com.kahya.kahyad.plist
REPO_ROOT := $(abspath .)

.PHONY: build test lint venv generate codesign install run-daemon install-agent uninstall-agent accept-w12
# sqlite_fts5 is required on EVERY Go build/test/lint/vet invocation:
# mattn/go-sqlite3's default build does not compile in FTS5, and
# kahyad/migrations/0002 (W12-03) creates an FTS5 virtual table that would
# fail at boot without it. Set once here (W12-02); never drop it.
GOTAGS := sqlite_fts5
# TEST_TAGS additionally builds in "e2e" (tests/e2e/w12_gate_test.go, the
# W12-10 hermetic W1-2 acceptance gate) so `make test` is the one command
# that actually runs it -- it must never be a build tag nobody invokes.
# That test builds/spawns real bin/kahyad, bin/kahya, bin/kahya-mcp and the
# real worker/.venv, which is exactly why `test` depends on both `venv` and
# `build` below (previously just `venv`): without `build` first, the
# hermetic gate would only ever hit its own "missing bin/* artifacts"
# t.Skip fallback, which is the "never actually run" failure mode this task
# explicitly warns against.
TEST_TAGS := $(GOTAGS),e2e
build:
	mkdir -p bin
	go build -tags $(GOTAGS) -ldflags "-X kahya/kahyad/internal/buildinfo.Version=$(KAHYA_VERSION)" -o bin/kahyad ./kahyad
	go build -tags $(GOTAGS) -o bin/kahya ./kahyad/cmd/kahya
	go build -tags $(GOTAGS) -o bin/kahya-mcp ./kahyad/cmd/kahya-mcp
venv:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(PY) -m pip install --quiet -r worker/requirements.lock
test: venv build
	go test -tags $(TEST_TAGS) ./...
	$(PY) -m unittest discover -s worker/tests -v
lint:
	test -z "$$(gofmt -l .)"
	go vet -tags $(GOTAGS) ./...
	go vet -tags $(TEST_TAGS) ./...
	if [ -f sqlc.yaml ]; then \
		go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate && \
		git diff --exit-code -- kahyad/internal/store/sqlcgen || \
			(echo "sqlc generate produced a diff — commit the regenerated code" && exit 1); \
	fi
generate:   # activated by W12-02 when sqlc.yaml lands
	if [ -f sqlc.yaml ]; then go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate; else echo "sqlc.yaml not yet present (W12-02)"; fi
codesign: build
	codesign -f -s "$(CODESIGN_ID)" bin/kahyad
install: codesign
	mkdir -p $(HOME)/bin
	install -m 0755 bin/kahyad $(HOME)/bin/kahyad
	install -m 0755 bin/kahya  $(HOME)/bin/kahya
run-daemon: build
	./bin/kahyad
install-agent: build
	mkdir -p "$(LAUNCH_AGENTS_DIR)"
	mkdir -p "$(KAHYA_LOG_DIR)"
	sed -e 's#__KAHYA_REPO_ROOT__#$(REPO_ROOT)#g' -e 's#__KAHYA_LOG_DIR__#$(KAHYA_LOG_DIR)#g' \
		kahyad/launchd/$(PLIST_NAME) > "$(LAUNCH_AGENTS_DIR)/$(PLIST_NAME)"
	launchctl bootstrap gui/$$(id -u) "$(LAUNCH_AGENTS_DIR)/$(PLIST_NAME)"
uninstall-agent:
	-launchctl bootout gui/$$(id -u)/com.kahya.kahyad
	rm -f "$(LAUNCH_AGENTS_DIR)/$(PLIST_NAME)"
# accept-w12: the W1-2 LIVE acceptance gate (W12-10). Unlike `test`'s
# hermetic e2e gate (mock Anthropic server, throwaway fixture corpus), this
# runs against a REAL already-running kahyad (launchd `make install-agent`
# or `make run-daemon` in another terminal), a real Anthropic credential/
# Claude Code session, and the real ~/Kahya seed corpus. See
# scripts/accept-w12.sh and docs/ipc.md's "W1-2 gate -- how to re-run"
# appendix.
accept-w12: build
	bash scripts/accept-w12.sh
