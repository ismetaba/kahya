# W12-09 — Python worker harness

**Status:** done — live E2E acceptance criterion deferred (see note below the acceptance list): no real Anthropic credential/Claude Code session available in this build environment.
**Phase:** W1–2 — Core
**Depends on:** W12-07, W12-08, W12-05
**Flags:** none
**Handoff refs:** §4 IPC ⚑ + model routing

## Goal
The real reasoning process. A Python worker that reads the task envelope from stdin, runs `ClaudeSDKClient` in streaming input mode, injects the `<hafiza>` block via a `UserPromptSubmit` hook, early-rejects tools via `can_use_tool` → `/policy/check` (fail-closed), obeys the envelope's model, and speaks W12-07's stdout protocol — completing the first end-to-end memory-answering loop.

## Context you need
The two binding IPC bullets (HANDOFF §4 ⚑, verbatim):

> - Worker `ClaudeSDKClient` + **streaming input modu** üzerine kurulur — `UserPromptSubmit` kancası ve `can_use_tool` geri-çağrısı tek-atımlık `query()` ile ÇALIŞMAZ.

> - Politika kontrolü: `~/Library/Application Support/Kahya/kahyad.sock` üzerinden **HTTP-over-UDS** `POST /policy/check`, timeout 5s; **her hata/timeout = RED (fail-closed)** — §5 "güvenlik yürütücüde" ilkesinin doğal sonucu.

`can_use_tool` is an early-reject/UX layer only (HANDOFF §5 ⚑): "`can_use_tool` bir **erken-ret/UX katmanıdır, güvenlik sınırı değildir** — worker sürecinin içinde çalışan bir SDK geri-çağrısıdır. Bağlayıcı politika kararı **kahyad'da** verilir…" — so a deny here is UX; the binding deny already lives in kahyad, and side-effect tools will verify approval tokens from W3-02 on.

Model routing (HANDOFF §4): "Model yönlendirme (karar **Go kodunda**, istemde değil)" — the worker NEVER chooses or changes the model; it passes `envelope["model"]` to the SDK verbatim and errors out if the envelope model is not in the §9 set {`claude-opus-4-8`,`claude-sonnet-5`,`claude-haiku-4-5`,`claude-fable-5`}.

Prompt-cache discipline (HANDOFF §4 ⚑ cost governor): "İstem önbelleği: donmuş sistem-öneki + araç tanımları, 1-saat TTL." — the system prompt is a frozen constant; per-task data (memory, prompt) enters via user messages/hook only.

Prior output: W12-07 defines envelope/env/stdout protocol (`docs/ipc.md`) and spawns this worker; W12-08 provides `ANTHROPIC_BASE_URL` (per-task listener) + the per-task proxy token `ANTHROPIC_API_KEY=kahya-task-<hex32>` (not a real key); W12-05 provides `/v1/memory/search` with `for_injection` + `hafiza_block` and the `bin/kahya-mcp` stdio bridge. W0-02 pinned `claude-agent-sdk` + lock file in `worker/`. Failure-posture nuance: policy check failures are DENY (security, fail-closed); memory-search failures during injection are **continue without injection + warn** (enrichment, not a security gate).

## Deliverables
- `worker/kahya_worker/__main__.py` — entrypoint: parse envelope → run session → emit stdout protocol.
- `worker/kahya_worker/envelope.py` — schema validation (reject unknown `schema_version`, missing fields, non-§9 model).
- `worker/kahya_worker/udshttp.py` — stdlib-only HTTP-over-UDS client (`http.client.HTTPConnection` over an `AF_UNIX` socket; no new deps).
- `worker/kahya_worker/hooks.py` — `UserPromptSubmit` hook + `can_use_tool` callback.
- `worker/kahya_worker/system_prompt.py` — the frozen system prefix (Turkish persona constant; see step 3).
- `worker/kahya_worker/logging.py` — JSONL to `$KAHYA_LOG_DIR/worker.jsonl`, `trace_id` (from `KAHYA_TRACE_ID`) on every line.
- `worker/tests/` — stdlib `unittest` suite (wired into `make test` alongside `go test`). NOTE: this bullet originally said "pytest" — corrected here per `tasks/README.md`'s protocol ("if a task file ever contradicts the handoff, the handoff wins — fix the task file in the same commit"): `Makefile`'s `test:` target has always run `python -m unittest discover -s worker/tests -v` (W0-02), never pytest, and `worker/requirements.lock` intentionally has no pytest dependency to add.

## Steps
1. Entrypoint: read one JSON envelope from stdin (then EOF), validate, configure logging. Any validation error → stdout `{"type":"error","message":"Görev zarfı geçersiz."}` + exit 2 (details to worker.jsonl in English).
2. Build `ClaudeAgentOptions`: `model` from envelope; `system_prompt` = frozen constant; `mcp_servers = {"kahya_memory": {"type":"stdio","command":"<bin/kahya-mcp>","env":{"KAHYA_SOCKET":…,"KAHYA_TRACE_ID":…}}}` (path from `KAHYA_MCP_BRIDGE` env, set by spawn — add it to W12-07's env table in `docs/ipc.md` in this task); `allowed_tools` = exactly `["mcp__kahya_memory__memory_search","mcp__kahya_memory__memory_write","mcp__kahya_memory__memory_forget"]` — NO SDK built-in file/exec tools (`Read`/`Glob`/`Grep`/`Bash`/…) in W1–2: kahyad's interim policy denies them anyway because file reads cannot be secret-lane-classified before W3-01's policy.yaml globs (§4 ⚑ ordering invariant); they return in W3. Keep the frozen system prompt AND this tool set byte-identical across all tasks — that stability IS the §4 ⚑ prompt-cache discipline ("donmuş sistem-öneki + araç tanımları, 1-saat TTL"); if the pinned SDK exposes cache-TTL configuration, pin 1h. `permission_mode` default (so `can_use_tool` fires); hooks per steps 3–4. Use `ClaudeSDKClient` with **streaming input** (async context manager + `client.query()` per prompt/`receive_response()` loop) — never the one-shot `query()` helper (the ⚑ above is explicit: hooks don't run there).
3. `UserPromptSubmit` hook: when `envelope["memory_injection"]` is true, POST `/v1/memory/search` `{"query": <prompt>, "k": 6, "for_injection": true, "task_id": …, "trace_id": …}` over UDS (timeout 5s). Non-empty `hafiza_block` → return it as `additionalContext` **byte-exact, unmodified** (kahyad already ledgered exactly these bytes — §5 safety #4; any local edit would break forensic equality). Search error/timeout → log `event=memory_search_failed` (warn) and continue without injection.
4. `can_use_tool(tool_name, tool_input, ctx)`: POST `/policy/check` with `{trace_id, task_id, session_id, tool_name, tool_input}`, timeout **5s**. Pass `tool_name` through verbatim — MCP tools arrive SDK-prefixed (`mcp__kahya_memory__memory_search`); kahyad canonicalizes (W12-07), the worker never rewrites names. `decision=="allow"` → allow; deny → deny with the server's Turkish `reason`. **Any** exception, timeout, non-200, or unparsable body → deny with `Politika kontrolü başarısız — reddedildi (fail-closed).` Log every decision (`event=tool_gate`, tool name, decision, duration_ms).
5. Streaming out: SDK text deltas → `{"type":"delta","text":…}`; on `init`/first message capture the SDK session id → `{"type":"session","session_id":…}` (kahyad persists it for W4 resume; do NOT implement resume); success → `{"type":"result","status":"ok"}` exit 0; SDK/API error → `{"type":"error","message":"Model çağrısı başarısız oldu. Ayrıntı: kahya log --trace <trace_id>"}` exit 1. stdout carries ONLY protocol lines; all diagnostics go to worker.jsonl/stderr.
6. Assert at startup that `ANTHROPIC_BASE_URL` is set and `ANTHROPIC_API_KEY` matches `^kahya-task-[0-9a-f]{32}$` (the per-task proxy token from W12-07/W12-08); if a real-looking key (`sk-ant-*`) is present anywhere in the environment, refuse to start (`event=real_key_in_env`, exit 2) — belt-and-braces for the §4 "API anahtarı worker'a verilmez" invariant.
7. Tests (stdlib `unittest`, no cloud calls — see the corrected Deliverables bullet above): envelope validation matrix incl. model not in §9 set; `udshttp` against a `socketserver` UDS fixture; hook injects fixture block byte-exact (assert exact bytes incl. Turkish `Kadıköy'de iki daire gezdik.`); hook continues on search timeout; `can_use_tool` matrix — allow, deny, timeout→deny, 500→deny, garbage-body→deny; stdout protocol framing (one JSON per line, no stray prints); real-key refusal. Mock the SDK client at the module boundary; record-replay SDK fixtures are W7-8 (`KAHYA_ENV=dev`), not here.

## Acceptance criteria
- [x] `make test` runs the `unittest` suite green (alongside Go tests) — 50 tests in `worker/tests/`, all passing; `worker/` lock file unchanged (`claude-agent-sdk` stays pinned — verified via `git diff --exit-code worker/requirements.lock`, the W0-02 lock file this repo actually uses).
- [x] Fail-closed proven by test: with kahyad's socket absent, `can_use_tool` denies in <6s with the exact Turkish fail-closed message (`worker/tests/test_hooks.py::TestCanUseTool::test_connection_refused_denies_fail_closed`).
- [ ] **DEFERRED — Live E2E** (key in Keychain, daemon + real W0-01 seed corpus up): `bin/kahya "iOS ev tasarım uygulamasına hangi Apple framework'üyle başlamayı planlamıştık?"` streams a Turkish answer that contains `RoomPlan`, exits 0. Not run: this build environment has no real Anthropic credential and no logged-in Claude Code SDK session to authenticate through (per the OWNER AUTH DECISION, `claude-agent-sdk` authenticates via that session, not a Keychain key), and exercising it also depends on confirming how the SDK's own session auth flows through kahyad's `passthrough`-mode forward-proxy live — untested by any CI-run test in this repo. Every other (hermetic) criterion on this list passes.
- [ ] **DEFERRED** (depends on the live run above): `jq`/single-trace-propagation check across `worker.jsonl` + `kahyad.jsonl`.
- [ ] **DEFERRED** (depends on the live run above): `sqlite3` check that `events.kind='model_call'` payload's `model` equals the envelope model.
- [ ] **DEFERRED** (depends on the live run above): `sqlite3` check that `tasks.session_id` is non-null after a real run.
- [x] `make lint` passes (`gofmt`/`go vet`/`sqlc` diff-check — this repo's W0-02 Python setup is `unittest` only, no separate Python linter configured beyond that; `worker/` imports cleanly under `python -m py_compile`).

## Out of scope
- Session resume, receipts, idempotency — W4-02. Taint tiers / Reader-Actor split — W4-03.
- Approval-token verification in MCP tools, ladder, WYSIWYE — W3-02/W3-06.
- Subagents, fan-out — post-core; "derin düşün" Fable-5 opt-in routing — W4-08 (fallback-beta shaping — W4-04).
- Any non-memory MCP server (fs/shell/AppleScript) — W3. `mlx-whisper` library use — W6-02.
- Worker-side retry of failed cloud calls — W4-04 owns the error taxonomy/backoff; here the worker fails fast and reports.
- Intent routing / model selection logic — Go-side (W4-08), out of the worker permanently.
