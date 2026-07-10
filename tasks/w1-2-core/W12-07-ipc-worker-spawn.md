# W12-07 — per-task worker spawn + /policy/check

**Status:** done
**Phase:** W1–2 — Core
**Depends on:** W12-01, W12-02 (tasks/events tables), W12-06 (SSE contract)
**Flags:** none
**Handoff refs:** §4 IPC ⚑, §4 routing ⚑ (ordering invariant), §5 safety enforcement plane ⚑

## Goal
kahyad can run a task: it spawns the Python worker per task with a JSON envelope on stdin and `trace_id` in the environment, relays the worker's stream to the CLI's `/v1/task` SSE contract, records task state + ledger events, and serves the fail-closed `POST /policy/check` endpoint with the interim static policy (R allowed, all W denied).

## Context you need
The IPC contract this task freezes (HANDOFF §4 ⚑, verbatim):

> - kahyad worker'ı **görev-başına** spawn eder: görev zarfı JSON stdin'den; `trace_id` env/arg ile geçer; W4 oturum devamı `session_id` ile.

> - Politika kontrolü: `~/Library/Application Support/Kahya/kahyad.sock` üzerinden **HTTP-over-UDS** `POST /policy/check`, timeout 5s; **her hata/timeout = RED (fail-closed)** — §5 "güvenlik yürütücüde" ilkesinin doğal sonucu.

Why the binding decision lives here and not in the worker (HANDOFF §5 ⚑, verbatim):

> ⚑ **Uygulama düzlemi (önce oku):** `can_use_tool` bir **erken-ret/UX katmanıdır, güvenlik sınırı değildir** — worker sürecinin içinde çalışan bir SDK geri-çağrısıdır. Bağlayıcı politika kararı **kahyad'da** verilir; yan-etkili MCP araçları kahyad'ın verdiği **tek-kullanımlık onay jetonunu** doğrulamadan yürümez (ya da yan-etkili MCP sunucularını kahyad spawn edip sahiplenir, worker onlara yalnız kahyad üzerinden erişir).

Interim static policy (backlog row, binding until W3): **R allowed, all W denied until W3 tasks land** — but "R allowed" is bounded by a second ⚑ that outranks the backlog wording (HANDOFF §4, verbatim):

> ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için; …

policy.yaml and its secret-lane globs do not exist until W3-01, so in W1–2 there is NO way to classify a file read — an allowed SDK `Read` would put unclassified file bytes into the next cloud call. The global fail-closed convention decides the tie: the only R tool the interim table may allow is `memory_search` (its corpus is the user-reviewed seed, whose cloud injection is the designed §6/§7 W1–2 acceptance). SDK `Read`/`Glob`/`Grep` return in W3 under policy.yaml.

Prior output: W12-01 server+config+logs; W12-02 the `tasks`/`events` tables; W12-06 defined the `/v1/task` SSE contract (deltas/result/error) — implement the server side to match it. The worker binary itself is W12-09 — develop against a fake worker script in testdata; wire the real command path via config.

## Deliverables
- `docs/ipc.md` — the frozen envelope + stdout protocol + `/policy/check` schema (this file IS the "IPC sözleşmesi W1–2'de sabitlenir" artifact).
- `kahyad/internal/spawn/spawn.go` + `spawn_test.go` — process lifecycle.
- `kahyad/internal/spawn/envelope.go` — envelope struct + validation.
- `kahyad/internal/policy/interim.go` + `interim_test.go` — static allow/deny table (W12-05's MCP gate creates this package first if it ran already — extend/reuse it, never fork a second table; replaced by W3-01/W3-02; keep the handler interface stable).
- UDS handlers: `POST /v1/task` (SSE per W12-06 contract), `POST /policy/check`.
- Config additions: `worker_cmd` (default `["<repo>/worker/.venv/bin/python","-m","kahya_worker"]`), `task_timeout_min` (already present, enforce here).

## Steps
1. Envelope v1 (document in `docs/ipc.md`; single JSON object written to worker stdin, then stdin closed):
   ```json
   {"schema_version":1, "task_id":"t_<hex>", "trace_id":"<hex32>", "session_id":null,
    "kind":"chat", "prompt":"<user text>", "model":"claude-sonnet-5",
    "memory_injection":true, "created_at":"<rfc3339>"}
   ```
   `model` comes from `cfg.default_model`, validated against the §9 cloud set {`claude-opus-4-8`,`claude-sonnet-5`,`claude-haiku-4-5`,`claude-fable-5`} — the routing decision is Go's (§4: "karar **Go kodunda**, istemde değil"); the full intent router lands in W4-08 — a static default is correct for W1–2. `session_id` is reserved for W4-02 resume — always present, null for new tasks.
2. Worker env (document in `docs/ipc.md`): `KAHYA_TASK_ID`, `KAHYA_TRACE_ID`, `KAHYA_SOCKET`, `KAHYA_LOG_DIR`, `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY=kahya-task-<hex32>` (a per-task random token, NOT a real key — the real key never leaves kahyad; W12-08's per-task listener rejects requests whose inbound key ≠ this token, so no other local process can spend through kahyad's key. Until W12-08 lands set base URL from `cfg.anthropic_upstream_url` behind a TODO). Start the process in its own process group (`Setpgid`) so W6-03 halt can kill the whole tree.
3. Worker stdout protocol (JSONL, one object per line — document it): `{"type":"delta","text":"…"}`, `{"type":"session","session_id":"…"}`, `{"type":"result","status":"ok"}`, `{"type":"error","message":"<Turkish user-facing>"}`. kahyad relays deltas to the `/v1/task` SSE stream, persists `session_id` onto the task row, and treats stderr as diagnostics (logged at warn).
4. `/v1/task` handler: validate prompt non-empty; use client-supplied `trace_id` (header/body) else mint; insert `tasks` row (`state='running'`, envelope JSON stored); ledger events `task_spawned` → (`task_done`|`task_error`|`task_timeout`); enforce `task_timeout_min` by killing the process group (state `error`, Turkish SSE error `Görev zaman aşımına uğradı (%d dk).`); on worker exit ≠0 without a `result` line ⇒ `task_error` + `Görev beklenmedik şekilde sonlandı. Ayrıntı: kahya log --trace %s`.
5. `POST /policy/check` request/response (freeze in `docs/ipc.md`):
   ```json
   {"trace_id":"…","task_id":"…","session_id":null,"tool_name":"Read","tool_input":{…}}
   → {"decision":"allow"|"deny","reason":"<Turkish, shown to user on deny>","rule":"interim-static-v1"}
   ```
   Canonicalize tool names first: the SDK reports MCP tools as `mcp__<server>__<tool>` (e.g. `mcp__kahya_memory__memory_search`) — strip the `mcp__<server>__` prefix before table lookup; built-in tools arrive bare. Interim table: allow exactly `{"memory_search"}`; deny **everything else** — including the R-class SDK built-ins `Read`, `Glob`, `Grep` (no secret-lane classification exists before W3-01's policy.yaml globs; §4 ordering invariant + fail-closed convention, see Context) — and `memory_write`, `memory_forget`, `Bash`, `WebFetch`, `WebSearch`, `Write`, `Edit`, unknown names (deny reason: `W3 politika altyapısı gelene dek yalnız hafıza araması (memory_search) açık.` / unknown: `Tanınmayan araç reddedildi (fail-closed).`). Malformed body ⇒ HTTP 400 with `{"decision":"deny",…}` — the body still says deny so a sloppy client can't parse an allow out of an error. Handler must answer well inside the caller's 5s budget: no I/O beyond one ledger insert (`kind='policy_decision'`, payload = request + decision).
6. Tests: fake worker scripts in `kahyad/internal/spawn/testdata/` (python3/sh) that (a) echo the envelope back as deltas — assert envelope bytes and env vars arrive intact, incl. Turkish prompt `Kadıköy'deki randevuyu hatırlat` byte-exact; (b) hang — assert timeout kill of the process group; (c) exit 3 mid-stream — assert `task_error` state + ledger. Policy tests: table-driven allow/deny incl. unknown tool and malformed JSON; every decision produces a `policy_decision` ledger row with the request's `trace_id`.

## Acceptance criteria
- [ ] `make test` green including spawn + interim policy tests.
- [ ] `docs/ipc.md` committed and complete (envelope, env, stdout protocol, policy schema, SSE contract cross-ref to W12-06).
- [ ] `curl -s --unix-socket … -XPOST http://kahyad/policy/check -d '{"trace_id":"t","task_id":"x","tool_name":"memory_search","tool_input":{}}' | jq -r .decision` → `allow`; same with `"tool_name":"mcp__kahya_memory__memory_search"` → `allow` (prefix canonicalized); with `"tool_name":"memory_write"` → `deny`; with `"tool_name":"Read"` → `deny` (§4 ordering invariant — no file reads before W3-01 globs); with body `not-json` → HTTP 400 and `.decision == "deny"`.
- [ ] With `worker_cmd` pointed at the echo fake: `bin/kahya "test sorusu"` streams the echoed text and exits 0; `sqlite3 brain.db "SELECT state FROM tasks ORDER BY created_at DESC LIMIT 1;"` → `done`; ledger has `task_spawned` and `task_done` with the same `trace_id` the CLI printed.
- [ ] All spawn/policy JSONL lines carry the task's `trace_id`: `bin/kahya log --trace <id>` shows spawn, policy (if any), and completion lines from kahyad.
- [ ] `ps` during the hang-fake test shows worker in its own process group; after timeout, no orphan processes remain (assert in test via `pgrep -g`).

## Out of scope
- The real Python worker (`ClaudeSDKClient`, hooks, `can_use_tool` client side) — W12-09.
- The forward-proxy and real `ANTHROPIC_BASE_URL` values — W12-08.
- Real policy (policy.yaml, ladder, approval tokens, 5-min undo) — W3-01/W3-02; do NOT build token issuance here, only the check endpoint shape.
- Session resume, receipts, retries, outbox dispatch — W4-02. Taint checks in policy decisions — W4-03.
- Intent router / model routing beyond the static default — W4-08 (kahyad-owned, §4).
