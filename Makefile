SQLC_VERSION ?= v1.30.0        # pin; bump deliberately, never 'latest'
VENV := worker/.venv
PY := $(VENV)/bin/python
# MLX_VENV is mlx/embed's OWN venv (W12-11) - separate process, separate
# deps from worker/'s (HANDOFF §4 three-process architecture). Never
# installed/activated by the plain `test`/`venv` targets: the embedding
# pipeline needs the real downloaded Qwen3-Embedding-0.6B model to do
# anything meaningful, so it is `test-mlx`'s dependency alone.
MLX_VENV := mlx/embed/.venv
MLX_PY := $(MLX_VENV)/bin/python

CODESIGN_ID ?= Kahya Dev

KAHYA_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
KAHYA_DATA_DIR := $(HOME)/Library/Application Support/Kahya
KAHYA_LOG_DIR := $(KAHYA_DATA_DIR)/logs
LAUNCH_AGENTS_DIR := $(HOME)/Library/LaunchAgents
PLIST_NAME := com.kahya.kahyad.plist
REPO_ROOT := $(abspath .)

# SANDBOX_IMAGE_TAG is the W3-04 shell sandbox image's tag - the exact
# value config.Config.DockerImageTag defaults to (kahyad/internal/config/
# config.go); mcp/shell.Runner refuses to run anything until the digest
# `sandbox-image` pins below matches what's actually built.
SANDBOX_IMAGE_TAG := kahya-sandbox:0.1.0

.PHONY: build test lint venv mlx-venv test-mlx generate codesign install run-daemon install-agent uninstall-agent accept-w12 accept-w4 eval-retrieval sandbox-image docker-up hammerspoon-install
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
	go build -tags $(GOTAGS) -o bin/kahya-trigger ./kahyad/cmd/kahya-trigger
venv:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(PY) -m pip install --quiet -r worker/requirements.lock
test: venv build
	@# W4-07 acceptance gate runs FIRST, before the main suite, so its
	@# anti-vacuous-green guard is ALWAYS reached: make aborts a target at the
	@# first recipe line that exits nonzero, so a failure in the main
	@# `go test ./...` below would otherwise skip this step entirely and the
	@# guard would never run (a dropped `acceptance` build tag while some
	@# other suite is red would be invisible). Fast (~17s) and it is the phase
	@# gate, so running it up front is the right order.
	@echo "running W4-07 + W6-04 acceptance gates (tests/acceptance/w4 + tests/acceptance/w6, CI-speed)..."
	@ACCEPT_OUT=$$(go test -tags $(GOTAGS),acceptance ./tests/acceptance/... -v 2>&1); \
	STATUS=$$?; \
	echo "$$ACCEPT_OUT"; \
	if echo "$$ACCEPT_OUT" | grep -q '\[no test files\]' || echo "$$ACCEPT_OUT" | grep -q 'no packages to test'; then \
		echo ""; \
		echo "ANTI-VACUOUS-GREEN GUARD TRIPPED: tests/acceptance/w4 or tests/acceptance/w6 reported '[no test files]' -- an acceptance gate did NOT actually run (a build-tag-gated test package that plain go test silently skips is a forbidden vacuously-green result, tasks/README.md gate rule). Check that -tags includes 'acceptance' and that tests/acceptance/{w4,w6}/*_test.go all carry the //go:build acceptance constraint." >&2; \
		exit 1; \
	fi; \
	if ! echo "$$ACCEPT_OUT" | grep -q 'TestW6Gate1VoiceLoopFullyLocal' || ! echo "$$ACCEPT_OUT" | grep -q 'TestW6Gate2HaltSurvivesDaemonRestart' || ! echo "$$ACCEPT_OUT" | grep -q 'TestW6Gate3PaletteAndFirstTokenLoggedToEvents'; then \
		echo ""; \
		echo "ANTI-VACUOUS-GREEN GUARD TRIPPED: one or more of the three named W6-04 gate tests (TestW6Gate1VoiceLoopFullyLocal / TestW6Gate2HaltSurvivesDaemonRestart / TestW6Gate3PaletteAndFirstTokenLoggedToEvents) did not appear in the acceptance test output at all (renamed/deleted/build-tag dropped?) -- tasks/README.md gate rule." >&2; \
		exit 1; \
	fi; \
	if [ $$STATUS -ne 0 ]; then \
		echo "W4-07/W6-04 acceptance gate FAILED (tests/acceptance/w4 + w6)" >&2; \
		exit 1; \
	fi
	@# W5-05 acceptance gate: the four hermetic gate tests (single-
	@# notification briefing, consolidation diff->author=kahyad commit,
	@# tainted-DENY/clean-ALLOW, mini-eval regression detection) MUST
	@# actually run, not silently no-op - same anti-vacuous-green guard
	@# shape as W4-07's above, scoped to the four packages the W5-05 gate
	@# lives in (kahyad/internal/eval is a brand-new package as of W5-05;
	@# the other three already had test files, but this still catches a
	@# future accidental //go:build tag or an emptied directory on any of
	@# them).
	@echo "running W5-05 acceptance gate (briefing/consolidation/policy/eval gate tests)..."
	@W5_OUT=$$(go test -tags $(GOTAGS) ./kahyad/internal/briefing/... ./kahyad/internal/consolidation/... ./kahyad/internal/policy/... ./kahyad/internal/eval/... -v 2>&1); \
	W5_STATUS=$$?; \
	echo "$$W5_OUT" | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'; \
	if echo "$$W5_OUT" | grep -q '\[no test files\]' || echo "$$W5_OUT" | grep -q 'no packages to test'; then \
		echo ""; \
		echo "ANTI-VACUOUS-GREEN GUARD TRIPPED: one of kahyad/internal/{briefing,consolidation,policy,eval} reported '[no test files]' -- the W5-05 acceptance gate did NOT actually run for that package (tasks/README.md gate rule: a test package that plain go test silently skips is a forbidden vacuously-green result)." >&2; \
		exit 1; \
	fi; \
	if ! echo "$$W5_OUT" | grep -q 'TestW5GateSingleNotificationTraceIDThenDuplicateSkipped' || ! echo "$$W5_OUT" | grep -q 'TestW5GateConsolidationProducesDiffThenApproveCommitsAsKahyaAndReindexes' || ! echo "$$W5_OUT" | grep -q 'TestSameToolSameTargetDeniedTaintedAllowedClean' || ! echo "$$W5_OUT" | grep -q 'TestRunnerRunDetectsInjectedRegression'; then \
		echo ""; \
		echo "ANTI-VACUOUS-GREEN GUARD TRIPPED: one or more of the four named W5-05 gate tests did not appear in the test output at all (renamed/deleted?) -- tasks/README.md gate rule." >&2; \
		exit 1; \
	fi; \
	if [ $$W5_STATUS -ne 0 ]; then \
		echo "W5-05 acceptance gate FAILED" >&2; \
		exit 1; \
	fi
	@if docker info >/dev/null 2>&1; then \
		echo "docker daemon detected -- exporting KAHYA_DOCKER_TESTS=1 (mcp/shell's container tests must PASS, never skip, from here on)"; \
		KAHYA_DOCKER_TESTS=1 go test -tags $(TEST_TAGS) ./...; \
	else \
		echo "docker daemon not available -- mcp/shell's container-requiring tests are NOT run (KAHYA_DOCKER_TESTS unset); suite stays green"; \
		go test -tags $(TEST_TAGS) ./...; \
	fi
	$(PY) -m unittest discover -s worker/tests -v
mlx-venv:
	test -d $(MLX_VENV) || python3 -m venv $(MLX_VENV)
	$(MLX_PY) -m pip install --quiet -r mlx/embed/requirements.txt
# test-mlx: the W12-11 LIVE-model gate. Separate from `test` on purpose -
# it needs the real downloaded Qwen3-Embedding-0.6B model (W0-03) and this
# machine's actual MLX runtime, neither of which a hermetic CI box can be
# assumed to have; `test` itself must stay green with no model present
# (search.Searcher's vector leg degrades to FTS-only automatically - see
# kahyad/internal/search's own degraded-fallback tests). Runs:
#   1. the "mlx"-tagged Go tests (kahyad/internal/mlxe2e's real end-to-end
#      cross-lingual gate - spawns a real bin/kahyad, which lazily spawns
#      the real mlx/embed/server.py);
#   2. mlx/embed's own pytest suite (test_server.py - skips cleanly if the
#      model snapshot is somehow missing, though `build`+`mlx-venv` above
#      being prerequisites here means it never should be).
test-mlx: mlx-venv build
	go test -tags $(GOTAGS),mlx ./... -v
	$(MLX_PY) -m pytest mlx/embed/test_server.py -v
# lint's single-egress-gate check (W3-05, HANDOFF S5 safety #1): every
# off-box byte must pass through kahyad/internal/egress.Gate.Check - this
# grep proves no OTHER code path in kahyad/mcp/worker dials out on its
# own. kahyad/internal/egress/ itself is exempt (it IS the gate). The
# short allowlist below is every net.Dial call this codebase had BEFORE
# W3-05 and still has after it - all of them dial a LOCAL UDS socket or
# 127.0.0.1 loopback, never an off-box host, so they are not egress at
# all: kahyad/cmd/kahya/client.go + kahyad/cmd/kahya-mcp/main.go +
# kahyad/cmd/kahya-trigger/main.go (the UDS control-socket clients),
# kahyad/internal/server/server.go's probeHealth (dials kahyad's own UDS
# socket) + its test, kahyad/internal/mlxe2e's test (dials the local MLX
# embed service's loopback TCP port), and
# mcp/shell/egress_integration_test.go's stand-in test proxy (this
# package cannot import kahyad/internal/egress - Go's internal-package
# boundary - so its Docker-integration test builds a small, independent,
# SAME-SHAPE stand-in proxy purely to drive the Docker network plumbing;
# see that file's own doc comment). Adding a NEW net.Dial/
# http.ProxyFromEnvironment anywhere else must fail this check until
# either it is routed through egress.Check or deliberately added here
# (reviewed in a commit) as another confirmed local-only exception.
lint:
	test -z "$$(gofmt -l .)"
	go vet -tags $(GOTAGS) ./...
	go vet -tags $(TEST_TAGS) ./...
	go vet -tags $(GOTAGS),mlx ./...
	go vet -tags $(GOTAGS),acceptance ./tests/acceptance/...
	if [ -f sqlc.yaml ]; then \
		go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate && \
		git diff --exit-code -- kahyad/internal/store/sqlcgen || \
			(echo "sqlc generate produced a diff — commit the regenerated code" && exit 1); \
	fi
	@echo "checking W3-05 single-egress-gate invariant..."
	@matches=$$(grep -rn 'http\.ProxyFromEnvironment\|net\.Dial' kahyad mcp worker 2>/dev/null \
		| grep -v '^kahyad/internal/egress/' \
		| grep -vE '^(kahyad/cmd/kahya/client\.go|kahyad/cmd/kahya-mcp/main\.go|kahyad/cmd/kahya-trigger/main\.go|kahyad/internal/server/server\.go|kahyad/internal/server/server_test\.go|kahyad/internal/mlxe2e/cross_lingual_test\.go|mcp/shell/egress_integration_test\.go):'); \
	if [ -n "$$matches" ]; then \
		echo "single-egress-gate violation: net.Dial/http.ProxyFromEnvironment found outside kahyad/internal/egress/ and the reviewed local-only allowlist (every OTHER off-box dial must go through egress.Check):"; \
		echo "$$matches"; \
		exit 1; \
	fi
generate:   # activated by W12-02 when sqlc.yaml lands
	if [ -f sqlc.yaml ]; then go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate; else echo "sqlc.yaml not yet present (W12-02)"; fi
# sandbox-image: builds the W3-04 shell sandbox image and pins its digest
# into docker/sandbox/IMAGE_DIGEST (committed) - mcp/shell.Runner refuses
# to run ANY shell_docker request until this file's content matches what
# `docker image inspect` reports for SANDBOX_IMAGE_TAG (supply-chain pin).
#
# DEVIATION (documented in docker/README.md too): a purely local build
# that has never been pushed to a registry has no RepoDigest -
# `docker images --digests` shows "<none>" for it (a well-known Docker
# quirk, not a bug here). This target pins the image ID instead (the
# sha256 of the image's config, via `docker image inspect --format
# {{.Id}}`), which changes on ANY layer/config change exactly like a real
# registry digest would - the identical supply-chain-pin security
# property, just sourced locally.
#
# BLOCKER 3 fix: --provenance=false --sbom=false. Without these, BuildKit
# attaches provenance/SBOM attestations to the image that embed a fresh
# build TIMESTAMP on every invocation - which changes the image's own
# config (and therefore `docker image inspect --format {{.Id}}`'s output)
# even when the Dockerfile and build context are byte-for-byte unchanged.
# That made the "supply-chain pin" a routine false-positive mismatch
# rather than a real tamper signal (every rebuild "drifted"). Disabling
# both attestations makes the image ID a pure function of the Dockerfile +
# build context again: an unchanged Dockerfile now yields the SAME digest
# across repeated `make sandbox-image` runs (verified: run it twice, diff
# docker/sandbox/IMAGE_DIGEST - no diff).
sandbox-image:
	docker build --provenance=false --sbom=false -t $(SANDBOX_IMAGE_TAG) -f docker/sandbox/Dockerfile docker/sandbox
	@DIGEST=$$(docker image inspect --format='{{.Id}}' $(SANDBOX_IMAGE_TAG)); \
	printf '%s\n' "$$DIGEST" > docker/sandbox/IMAGE_DIGEST; \
	echo "sandbox image built: $(SANDBOX_IMAGE_TAG) $$DIGEST"
	docker images --digests $(SANDBOX_IMAGE_TAG)
# docker-up: starts colima (per docker/README.md) if `docker info` isn't
# already answering; a no-op against an already-running Docker Desktop.
docker-up:
	@if docker info >/dev/null 2>&1; then \
		echo "docker already up"; \
	else \
		command -v colima >/dev/null 2>&1 || { echo "colima not installed - see docker/README.md"; exit 1; }; \
		colima start --cpu 4 --memory 8 --vm-type vz; \
	fi
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
# hammerspoon-install (W6-01): copies hammerspoon/kahya.lua ->
# ~/.hammerspoon/kahya.lua, substituting the @KAHYA_BIN@ placeholder with
# the ABSOLUTE path of the built `kahya` CLI ($(abspath bin/kahya) -
# hs.task cannot PATH-resolve a bare command name, Hammerspoon's GUI launch
# environment has no shell PATH at all), then appends `require("kahya")`
# to ~/.hammerspoon/init.lua if not already present (creating init.lua
# first if it doesn't exist yet). Depends on `build` so bin/kahya actually
# exists before its path is baked into the installed Lua file.
hammerspoon-install: build
	mkdir -p "$(HOME)/.hammerspoon"
	sed -e 's#@KAHYA_BIN@#$(abspath bin/kahya)#g' \
		hammerspoon/kahya.lua > "$(HOME)/.hammerspoon/kahya.lua"
	touch "$(HOME)/.hammerspoon/init.lua"
	grep -qxF 'require("kahya")' "$(HOME)/.hammerspoon/init.lua" || \
		echo 'require("kahya")' >> "$(HOME)/.hammerspoon/init.lua"
	@echo "installed: $(HOME)/.hammerspoon/kahya.lua (kahya bin: $(abspath bin/kahya))"
	@echo "reload Hammerspoon's config (menu bar icon -> Reload Config) to pick it up"
# accept-w12: the W1-2 LIVE acceptance gate (W12-10). Unlike `test`'s
# hermetic e2e gate (mock Anthropic server, throwaway fixture corpus), this
# runs against a REAL already-running kahyad (launchd `make install-agent`
# or `make run-daemon` in another terminal), a real Anthropic credential/
# Claude Code session, and the real ~/Kahya seed corpus. See
# scripts/accept-w12.sh and docs/ipc.md's "W1-2 gate -- how to re-run"
# appendix.
accept-w12: build
	bash scripts/accept-w12.sh
# accept-w4: the W4-07 durability acceptance gate (HANDOFF §6 W4) - an
# orchestrated REAL-daemon (dev profile) run of scenarios A (kill-resume
# no-double-execution), B (offline -> reconnect), and C (ledger tamper vs
# remote anchor). Fast by default (~90s); W4_REAL=1 switches scenario A to
# the real-time (>=600s) evidence run tasks/w4-durability/
# W4-07-w4-acceptance.md's own acceptance criteria require once. Cloud-free
# and headless - no real Anthropic session/API key/Keychain item needed
# (scripted fixture workers + a local fake upstream + KAHYA_ANCHOR_KEY_
# OVERRIDE's dev-only escape hatch stand in for all three).
accept-w4: build
	bash scripts/accept_w4.sh
# eval-retrieval: the W78-01 LIVE retrieval-QA drill (§5-Memory-#5 pre-change
# gate). Like accept-w4/accept-w12 it runs against a REAL already-running
# kahyad (launchd `make install-agent` or `make run-daemon` in another
# terminal) and the real ~/Kahya seed corpus + private retrieval dataset - it
# is NOT part of hermetic `make test` (which fully proves the runner/scorer/
# gate LOGIC with the synthetic testdata fixture instead). `kahya eval
# retrieval` triggers the in-daemon run over the UDS, ledgers one
# eval.retrieval.result event, prints a Turkish precision table, and exits
# non-zero when precision < 0.80 (çekimserlik dahil).
eval-retrieval: build
	./bin/kahya eval retrieval
