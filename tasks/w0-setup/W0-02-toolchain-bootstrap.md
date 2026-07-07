# W0-02 — Toolchain bootstrap

**Status:** done
**Phase:** W0 — Day-1 setup
**Depends on:** none
**Flags:** none
**Handoff refs:** §4 stack, §9

## Goal

The code repo (`~/code/kahya`) builds, tests, and lints with one command each. Go module with
`goose` (library) and pinned `sqlc` scaffolding; Python worker env with exact-pinned
`claude-agent-sdk` and a lock file; a `Makefile` with `build`/`test`/`lint`; and a minimal test
harness (one Go test + one Python test) that later tasks extend into the §5 invariant CI suite.

## Context you need

HANDOFF §4 stack table (binding, quote verbatim):

> Kontrol düzlemi | **Go** + `sqlc` üretimli sorgular. ⚑ **Migrasyon: `goose`/`golang-migrate`** (sqlc migrasyon yapmaz), kahyad açılışında her işten önce koşar; sürüm `PRAGMA user_version`

> Ajan runtime | **Python** + `claude-agent-sdk` (sürüm sabitlenir + lock dosyası)

HANDOFF §9:

> **Anahtar kütüphaneler:** `claude-agent-sdk` (Python, sürüm pinli), MCP, `sqlc` + **`goose`/`golang-migrate`**, `sqlite-vec` (≥0.1.9), `robfig/cron/v3`, `mlx-whisper`, `mlx_lm.server`, Hammerspoon, Telegram bot **`gopkg.in/telebot.v4` (Go — kahyad içinde, WYSIWYE onay kapısının parçası; grammY/TS DEĞİL — iki-süreç yığınıyla çelişir)**.

Decisions fixed here (no alternatives): migration tool = **`github.com/pressly/goose/v3`** as a
Go library (BACKLOG W12-02 says "goose migrations run at kahyad startup"); sqlc invoked via
version-pinned `go run` (no global install); Python deps via `python3 -m venv` + `pip` with
`worker/requirements.in` (intent) and `worker/requirements.lock` (frozen). Repo layout is
HANDOFF §7: `kahyad/` (daemon), `kahyad/cmd/kahya/` (CLI), `mcp/memory/` (Go, inside kahyad),
`worker/` (Python). One Go module at repo root so `kahyad` and `mcp/memory` share code.

Gotcha: W0-04 signs `bin/kahyad` and attaches Keychain ACLs to it, so this task MUST produce a
buildable (stub) `kahyad` binary; W12-01 replaces the stub internals.

## Deliverables

- `/Users/matt/code/kahya/go.mod` + `go.sum` — module `kahya`, dep `github.com/pressly/goose/v3`.
- `/Users/matt/code/kahya/kahyad/main.go` — stub main (prints version string, exits 0).
- `/Users/matt/code/kahya/kahyad/cmd/kahya/main.go` — stub CLI main (same).
- `/Users/matt/code/kahya/kahyad/internal/buildinfo/buildinfo.go` (+ `buildinfo_test.go`) —
  `const Version = "0.0.0-dev"` and a test asserting it is non-empty.
- `/Users/matt/code/kahya/kahyad/db/migrate.go` — placeholder that keeps the goose dep
  referenced (`var _ = goose.SetBaseFS`, see step 3); W12-02 replaces it with the real
  run-at-startup migration runner.
- `/Users/matt/code/kahya/kahyad/db/migrations/.gitkeep` — goose migrations dir (empty; W12-02 fills).
- `/Users/matt/code/kahya/kahyad/db/queries/.gitkeep` — sqlc queries dir (empty; W12-02 fills).
- `/Users/matt/code/kahya/worker/requirements.in` + `worker/requirements.lock` — exact
  `claude-agent-sdk==<version>` pin.
- `/Users/matt/code/kahya/worker/tests/test_smoke.py` — stdlib `unittest`, imports `claude_agent_sdk`.
- `/Users/matt/code/kahya/Makefile` — `build`, `test`, `lint`, `venv`, `generate` (inert until W12-02).
- Updated `/Users/matt/code/kahya/.gitignore` — `bin/`, `worker/.venv/`, `__pycache__/`, `*.pyc`.

## Steps

1. `cd /Users/matt/code/kahya && go mod init kahya` (skip if go.mod exists). Record the
   installed Go version in go.mod (`go mod edit -go=$(go env GOVERSION | sed 's/go//')`).
2. Create the stub mains and `buildinfo` package per Deliverables. Both mains print
   `kahyad <Version> (stub; replaced by W12-01)` / `kahya <Version> (stub; replaced by W12-06)`
   and exit 0 — English, these are logs/dev output, not user-facing UX.
3. `go get github.com/pressly/goose/v3@latest`, then note the resolved version — it is now
   pinned by go.mod/go.sum. Add a blank import is NOT needed; instead add
   `kahyad/db/migrate.go` with a placeholder function `//nolint` that references
   `goose.SetBaseFS` behind a `var _ = goose.SetBaseFS` so the dep survives `go mod tidy`.
4. Create the migrations/queries dirs with `.gitkeep`.
5. Write the `Makefile` (tabs, not spaces):
   ```make
   SQLC_VERSION ?= v1.30.0        # pin; bump deliberately, never 'latest'
   VENV := worker/.venv
   PY := $(VENV)/bin/python

   .PHONY: build test lint venv generate
   build:
   	mkdir -p bin
   	go build -o bin/kahyad ./kahyad
   	go build -o bin/kahya ./kahyad/cmd/kahya
   venv:
   	test -d $(VENV) || python3 -m venv $(VENV)
   	$(PY) -m pip install --quiet -r worker/requirements.lock
   test: venv
   	go test ./...
   	$(PY) -m unittest discover -s worker/tests -v
   lint:
   	test -z "$$(gofmt -l .)"
   	go vet ./...
   generate:   # activated by W12-02 when sqlc.yaml lands
   	if [ -f sqlc.yaml ]; then go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate; else echo "sqlc.yaml not yet present (W12-02)"; fi
   ```
   (The `if` form is deliberate — a `test -f … && sqlc … || echo` one-liner would swallow real
   sqlc failures with exit 0 once sqlc.yaml exists.)
   Before committing, check the pinned `SQLC_VERSION` is a real release
   (`go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 version` — if it fails, use the latest
   existing release tag and keep it pinned).
6. Python env: `python3 -m venv worker/.venv && worker/.venv/bin/pip install claude-agent-sdk`.
   Then pin: `worker/.venv/bin/pip show claude-agent-sdk` → write
   `claude-agent-sdk==<exact version>` into `worker/requirements.in`, and
   `worker/.venv/bin/pip freeze > worker/requirements.lock`.
7. Write `worker/tests/test_smoke.py`: a `unittest.TestCase` that (a) imports
   `claude_agent_sdk`, (b) reads `worker/requirements.lock` and asserts a line starts with
   `claude-agent-sdk==` (lock discipline is itself under test).
8. Update `.gitignore` per Deliverables (append; keep existing entries).
9. Run `go mod tidy`, then `make build`, `make test`, `make lint` — all must pass.
10. Commit: `[W0-02] bootstrap Go module, goose/sqlc scaffolding, pinned Python worker env`.

## Acceptance criteria

- [ ] `make build` exits 0 and produces `bin/kahyad` and `bin/kahya`; `./bin/kahyad` prints the
      stub version line and exits 0.
- [ ] `make test` exits 0 and runs BOTH the Go test (`buildinfo`) and the Python smoke test
      (visible in output).
- [ ] `make lint` exits 0 (gofmt clean + `go vet` clean).
- [ ] `grep 'github.com/pressly/goose/v3' /Users/matt/code/kahya/go.mod` matches (pinned via go.sum).
- [ ] `grep -E '^claude-agent-sdk==[0-9]' /Users/matt/code/kahya/worker/requirements.lock` matches
      (exact pin, no range).
- [ ] `grep 'SQLC_VERSION' /Users/matt/code/kahya/Makefile` matches a pinned `v*` version.
- [ ] `git -C /Users/matt/code/kahya status --porcelain` after commit shows nothing —
      in particular `worker/.venv/` and `bin/` are ignored, not tracked.

## Out of scope

- Any real daemon logic: config, UDS listener, JSONL `trace_id` logging, launchd plist (W12-01).
- `sqlc.yaml`, actual migrations/queries, the §5 schema, WAL/PRAGMA policy (W12-02).
- `sqlite-vec`, FTS5 (W12-03); `robfig/cron` wiring (W4-01); `telebot.v4` (W3-07).
- codesign step and `make install` (W0-04 adds them to this Makefile).
- Worker harness logic — `ClaudeSDKClient`, hooks, `can_use_tool` (W12-09); MCP servers (W12-05).
- CI workflow files (W78-03 collects the invariant tests into CI).
