# W12-01 — kahyad skeleton

**Status:** todo
**Phase:** W1–2 — Core
**Depends on:** W0-02
**Flags:** none
**Handoff refs:** §4 architecture+IPC

## Goal
A running, launchd-supervised Go daemon. After this task `kahyad` builds from this repo, listens HTTP-over-UDS on the contract socket, serves `GET /health`, writes JSONL logs with a `trace_id` on every line, shuts down gracefully, and is kept alive by a LaunchAgent. Every later W12 task mounts onto this skeleton.

## Context you need
kahyad's role (HANDOFF §4, verbatim):

> - **`kahyad` (Go daemon)** — kontrol düzlemi. launchd LaunchAgent (`KeepAlive=true`). Sahip olduğu: niyet yönlendirici, görev/saga durum makinesi (kademeli yürütme), politika motoru, maliyet valisi, defter (append-only events), zamanlayıcı, ve **SQLite hafıza indeksi**. **Keychain'den bulut anahtarını okuyan tek süreç.**

The socket path is fixed by the IPC contract (HANDOFF §4 ⚑) — this task creates the server that `/policy/check` (W12-07) will mount on:

> - Politika kontrolü: `~/Library/Application Support/Kahya/kahyad.sock` üzerinden **HTTP-over-UDS** `POST /policy/check`, timeout 5s; **her hata/timeout = RED (fail-closed)** — §5 "güvenlik yürütücüde" ilkesinin doğal sonucu.

Logging is acceptance-bearing (HANDOFF §4 ⚑):

> - Tüm süreçler her satırda `trace_id` içeren **JSONL** loglar — W1–2 kabul kriteri ancak böyle ölçülebilir.

Paths stay ASCII (HANDOFF §7 ⚑):

> ⚑ **Dizin adları ASCII** (`~/Kahya`) — non-ASCII `â` policy.yaml globlarında ve Docker/SQLite bayt-düzeyi karşılaştırmalarında NFC/NFD sessiz uyuşmazlık riski taşır; "Kâhya" yalnızca ürün/görünen ad.

Prior output you build on: W0-02 created the Go module, Makefile (`build`/`test`/`lint`) and test harness. If W0-04 is already done, keep the `codesign -s 'Kahya Dev'` Makefile step on every build (Go's ad-hoc signature changes per build and breaks Keychain ACLs).

## Deliverables
- `kahyad/main.go` — entrypoint: load config → init logger → start UDS HTTP server → block on SIGTERM/SIGINT → graceful shutdown.
- `kahyad/internal/config/config.go` + `config_test.go` — typed config: defaults → optional YAML file → env overrides.
- `kahyad/internal/logx/logx.go` + `logx_test.go` — JSONL logger (wraps `log/slog` JSON handler).
- `kahyad/internal/traceid/traceid.go` — mint trace_ids: 16 random bytes, lowercase hex (32 chars).
- `kahyad/internal/server/server.go` + `server_test.go` — HTTP-over-UDS server + `GET /health`.
- `kahyad/launchd/com.kahya.kahyad.plist` — LaunchAgent template (committed; installed by make target, never by a model-driven tool — `~/Library/LaunchAgents/**` is a §5 #6 deny glob).
- Makefile additions: `run-daemon`, `install-agent`, `uninstall-agent`.

## Steps
1. `internal/config`: fields + defaults — `data_dir` = `~/Library/Application Support/Kahya`; `socket` = `<data_dir>/kahyad.sock`; `log_dir` = `<data_dir>/logs`; `db_path` = `<data_dir>/brain.db`; `memory_dir` = `~/Kahya/memory`; `anthropic_upstream_url` = `https://api.anthropic.com`; `embed_port` = `8092`; `default_model` = `claude-sonnet-5`; `task_timeout_min` = `30`; `active_embed_model_ver` = `qwen3-embedding-0.6b:512:v1`. Load order: defaults → `<data_dir>/config.yaml` if present (parse with `gopkg.in/yaml.v3`, pinned in go.mod) → env overrides `KAHYA_DATA_DIR`, `KAHYA_SOCKET`, `KAHYA_MEMORY_DIR`, `KAHYA_DB_PATH`, plus `KAHYA_ENV` (`prod` default | `dev`) surfaced as `cfg.Env` (these exist so tests and the W7–8 `KAHYA_ENV=dev` profile can redirect every path; any dev-only behavior elsewhere — e.g. W12-08's Keychain override for the hermetic gate — MUST check `cfg.Env == "dev"` and be inert in prod). Expand `~`; **fail startup if any configured path contains a non-ASCII rune** (§7 ⚑ above).
2. `internal/logx`: JSON lines to `<log_dir>/kahyad.jsonl` (append, create dirs 0700) **and** stderr. Required keys on every line: `ts` (RFC3339Nano), `level`, `event`, `trace_id`. Mint one boot trace_id at startup; lines outside any request/task context carry it — no line may ever have empty/missing `trace_id`. Provide `logx.With(traceID)` returning a scoped logger.
3. `internal/server`: `net.Listen("unix", cfg.Socket)`. Before binding: if socket file exists, dial it — a live daemon answers `/health` ⇒ log `"event":"already_running"` and exit 1; dead socket file ⇒ unlink and bind. `chmod 0600` the socket. Serve `http.Server` with `ReadHeaderTimeout` 5s. Route `GET /health` → `200 {"status":"ok","pid":<pid>,"uptime_s":<n>,"version":"<git describe>"}`. Every request handled gets a trace_id: from `X-Kahya-Trace-Id` header if present, else minted; logged via middleware (`event=http_request`, path, status, duration_ms).
4. Graceful shutdown: on SIGTERM/SIGINT — stop accepting, `http.Server.Shutdown` with 5s context, unlink socket, final line `"event":"shutdown_complete"`, exit 0.
5. `kahyad/launchd/com.kahya.kahyad.plist`: `Label` `com.kahya.kahyad`, `ProgramArguments` → absolute path to `<repo>/bin/kahyad`, `KeepAlive` `true`, `RunAtLoad` `true`, `StandardOutPath`/`StandardErrorPath` → `<log_dir>/launchd-kahyad.{out,err}.log`. `install-agent` target: substitute repo path into the template, copy to `~/Library/LaunchAgents/`, `launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.kahya.kahyad.plist`; `uninstall-agent`: `launchctl bootout gui/$UID/com.kahya.kahyad` + remove plist.
6. Tests: config precedence (default < file < env) and non-ASCII path rejection; logger emits valid JSON with all four required keys; server test (unix socket in `t.TempDir()`): health returns 200, stale-socket takeover works, second instance refuses.

## Acceptance criteria
- [ ] `make build` produces `bin/kahyad`; `make test` and `make lint` green including the three new packages.
- [ ] With the daemon running: `curl -s --unix-socket "$HOME/Library/Application Support/Kahya/kahyad.sock" http://kahyad/health | jq -e '.status=="ok"'` exits 0.
- [ ] `jq -es 'all(.trace_id != null and .trace_id != "")' < "$HOME/Library/Application Support/Kahya/logs/kahyad.jsonl"` prints `true` (every line carries trace_id — §4 ⚑).
- [ ] `kill -TERM <pid>` ⇒ log contains `"event":"shutdown_complete"`, socket file is gone, exit status 0.
- [ ] `make install-agent` then `launchctl print gui/$UID/com.kahya.kahyad` shows the service running; `kill -9 <pid>` ⇒ launchd respawns it (new pid, `/health` answers again) — `KeepAlive=true` verified.
- [ ] Starting a second `kahyad` while one runs exits non-zero and logs `"event":"already_running"`.
- [ ] `stat -f '%Lp' "$HOME/Library/Application Support/Kahya/kahyad.sock"` prints `600`.

## Out of scope
- SQLite/migrations/schema — W12-02. `/policy/check` handler — W12-07. Forward-proxy — W12-08. Search/reindex endpoints — W12-03/W12-04.
- Scheduler (launchd `StartCalendarInterval` jobs, in-daemon cron) — W4-01.
- Telegram, Hammerspoon, any notification delivery — W3-07/W6-01.
- Creating the `Kahya Dev` signing identity or Keychain items — W0-04.
