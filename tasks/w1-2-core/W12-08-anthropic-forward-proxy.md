# W12-08 — Anthropic forward-proxy + cost governor

**Status:** todo
**Phase:** W1–2 — Core
**Depends on:** W12-01, W0-04
**Flags:** none
**Handoff refs:** §4 IPC ⚑ + cost governor ⚑

## Goal
The worker reaches Claude only through kahyad. A localhost forward-proxy injects the API key from Keychain, attributes every request to a task, counts tokens and dollars, enforces the 500K/task ceiling and the $10/day–$150/month budgets, records the cache-hit metric, and exposes alarm hooks (delivery wired to Telegram later in W3-07).

## Context you need
The binding IPC bullet (HANDOFF §4 ⚑, verbatim):

> - **API anahtarı worker'a verilmez:** kahyad localhost'ta auth-header ekleyen bir **forward-proxy** dinler ve worker'ı `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` ile spawn eder. **Maliyet valisi, cache-hit metriği ve model-çağrısı egress kapısı bu proxy noktasında uygulanır.**

The concrete governor (HANDOFF §4 ⚑, verbatim):

> ⚑ **Maliyet valisi (somut):** görev-başına 500K token tavanı; günlük bütçe $10 / aylık $150. Tavanda görev **duraklar** + Telegram bildirimi; günlük bütçenin %80'inde yönlendirici bir kademe ucuza düşer (Opus→Sonnet→yerel). Cache-hit oranı ve günlük harcama **alarm verir** (Telegram'a) — sessiz cache-bozan maliyeti 5–10× katlar. İstem önbelleği: donmuş sistem-öneki + araç tanımları, 1-saat TTL.

kahyad is "**Keychain'den bulut anahtarını okuyan tek süreç**" (§4). Keychain failure mode (HANDOFF §7): "Keychain kilitli/erişilemezse (SSH oturumu, lock-keychain zaman aşımı → `errSecInteractionNotAllowed`): bulut şeridi **fail-fast + kullanıcı bildirimi**, yerel gizli-şerit çalışmaya devam eder."

Pricing (HANDOFF §4 table): `claude-opus-4-8` $5/$25 · `claude-sonnet-5` $3/$15 (intro $2/$10 until 2026-08-31) · `claude-haiku-4-5` $1/$5 · `claude-fable-5` $10/$50, per MTok in/out.

Task attribution design (fixes `<port>` in the quote): kahyad opens a **per-task ephemeral listener** (`127.0.0.1:0`) when spawning a worker; every request on that listener is tagged with that task_id/trace_id and funneled through the shared governor to `cfg.anthropic_upstream_url`. No custom headers or path prefixes needed from the SDK; the listener closes when the task ends. W12-07's spawn TODO switches to `ANTHROPIC_BASE_URL=http://127.0.0.1:<ephemeral-port>`. The per-task `ANTHROPIC_API_KEY=kahya-task-<hex32>` token (W12-07 env) doubles as listener auth: 127.0.0.1 is a shared surface, and without the token check ANY local process could discover the port and spend through kahyad's real key — a hole in "**API anahtarı worker'a verilmez**" and in cost-governor attribution.

Prior output: W0-04 created Keychain item `kahya.anthropic` with ACL for the codesigned kahyad. `events` table from W12-02 (execute in backlog order). Model-call **egress gating** at this proxy point becomes real in W3-05; leave a named hook (`egressGate(req) error`, returns nil for `api.anthropic.com` upstream for now).

## Deliverables
- `kahyad/internal/anthproxy/proxy.go` + `proxy_test.go` — per-task listener + reverse proxy (stdlib `net/http/httputil`).
- `kahyad/internal/anthproxy/governor.go` + `governor_test.go` — budgets, ceilings, downgrade flag, cache metric.
- `kahyad/internal/anthproxy/usage.go` — SSE/JSON usage extraction; pricing table with intro-window logic.
- `kahyad/internal/secrets/keychain.go` + test (test skips if identity absent) — read `kahya.anthropic` via `/usr/bin/security find-generic-password -s kahya.anthropic -a kahya -w`, cached in memory, never logged.
- `kahyad/internal/notify/notify.go` — `Notifier` interface + JSONL/ledger implementation (Telegram impl in W3-07).
- Wiring in spawn (W12-07 code): create listener before spawn, close after exit.

## Steps
1. Proxy handler: reject any request whose inbound `x-api-key`/`authorization` does not carry this task's `kahya-task-<hex32>` token — `401` Anthropic-shaped error + ledger `proxy_auth_reject` (see Context: localhost auth). Then strip ALL inbound auth headers; set `x-api-key` from Keychain; forward method/path/body to `cfg.anthropic_upstream_url`; stream the response back unbuffered (SSE must flow token-by-token). If the key is unreadable at first use: respond `503` with Anthropic-shaped error JSON (`{"type":"error","error":{"type":"api_error","message":"Keychain erişilemiyor — bulut şeridi kapalı"}}`), notify once (`event=keychain_unavailable`), keep serving locals (fail-fast for the cloud lane only, §7 quote above). Key source: Keychain is the ONLY production source; when `cfg.Env == "dev"` (W12-01 `KAHYA_ENV`) an explicit `KAHYA_ANTHROPIC_KEY_OVERRIDE` env value may substitute — required by W12-10's hermetic gate (mock upstream, no Keychain on CI); ignored with a loud `"event":"key_override_ignored"` warn line in prod.
2. Usage extraction per `/v1/messages` call: non-stream → response JSON `usage`; stream → parse SSE `message_start` (input_tokens, cache_creation_input_tokens, cache_read_input_tokens) + final `message_delta` (output_tokens). Compute USD via the pricing table (intro Sonnet until `2026-08-31`, then standard — implement as dated rows, not a flag; each row carries explicit `usd_in`, `usd_out`, `usd_cache_read`, `usd_cache_write_1h` columns — cache reads are discounted and **1h-TTL cache writes are premium-billed** relative to base input; take the exact multipliers from the current Anthropic pricing docs at implementation time, do not guess). Ledger event `kind='model_call'`, payload `{task_id, model, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, usd, status, duration_ms}` with the task's `trace_id`.
3. Governor (all state derived from `events` at boot — `SELECT` sums for today/this month/per task — then maintained in memory):
   - **Per-task ceiling:** running token sum (input+output+cache_creation) per task; if a new request would exceed **500_000**, do not forward: return the 503-shaped error with message `Görev token tavanına ulaştı (500K) — duraklatıldı.`, set task state `paused_budget` (task row update via a callback into spawn/store, keeping anthproxy store-agnostic), ledger `task_paused_budget`, `Notifier.Alarm(...)`.
   - **Daily/monthly budget:** same block behavior at $10/day, $150/month (block message: `Günlük bütçe doldu ($10).` / `Aylık bütçe doldu ($150).`).
   - **80% downgrade rung:** when today's spend crosses $8, set `Downgraded()` true; W12-07's envelope builder consults it for NEW tasks. The chain is fixed by the ⚑ verbatim — **Opus→Sonnet→yerel**: `claude-opus-4-8`→`claude-sonnet-5`; `claude-sonnet-5`→local lane. Do NOT invent a Sonnet→Haiku rung (Haiku is a task-class model in the §4 routing table, not a rung in this chain). The "→yerel" rung needs W3-08's local lane: until it lands, Sonnet-class tasks stay on Sonnet and kahyad ledgers `budget_downgrade_unavailable` once per day — document the gap in code + `docs/ipc.md`. Ledger `budget_downgrade_on` once per day.
   - **Alarms:** daily spend crossing 80%/100%, and daily cache-hit ratio `< cfg.cache_hit_alarm_threshold` (default 0.5) once ≥20 calls that day. `Notifier` logs + ledgers now; W3-07 adds Telegram delivery.
4. Cache metric + cache-buster detection: cache_hit = `cache_read_input_tokens / max(1, input_tokens + cache_read_input_tokens)` per call, aggregated daily. Also sha256 the request's `system[0]` block (if present); if it changes more than twice in a day, ledger `cache_buster_suspect` (İstem önbelleği discipline is enforced worker-side in W12-09: frozen system prefix + tool defs, 1h TTL; the proxy only detects violations).
5. Config additions: `daily_budget_usd` 10, `monthly_budget_usd` 150, `task_token_ceiling` 500000, `downgrade_at_ratio` 0.8, `cache_hit_alarm_threshold` 0.5 (defaults match HANDOFF; committed in code).
6. Tests: fake upstream `httptest.Server` asserting (a) inbound task token is stripped and replaced with the upstream key; (b) a request with a missing/wrong token gets `401` + `proxy_auth_reject` ledger row and the upstream records zero hits; (c) SSE streams through unbuffered (delta timing preserved via flusher); usage parsing fixtures for stream + non-stream; governor unit tests — ceiling blocks the 500K-crossing request, $-budgets block, 80% flips downgrade (Opus→Sonnet; Sonnet unchanged + `budget_downgrade_unavailable` until W3-08), boot-time rebuild from a fixture events table matches in-memory totals; per-task listener closes on task end (connection refused after); `KAHYA_ANTHROPIC_KEY_OVERRIDE` honored only when `cfg.Env=="dev"`, ignored + warn-logged in prod.

## Acceptance criteria
- [ ] `make test` green including all step-6 tests; no test requires a real API key (Keychain test skips cleanly if `kahya.anthropic` absent).
- [ ] Live check (key present): run a task via `bin/kahya` with `worker_cmd` = a fake worker that curls `$ANTHROPIC_BASE_URL/v1/messages` with header `x-api-key: $ANTHROPIC_API_KEY` and a 1-token Haiku request — response 200, and `sqlite3 brain.db "SELECT json_extract(payload,'$.model'), json_extract(payload,'$.usd') FROM events WHERE kind='model_call' ORDER BY id DESC LIMIT 1;"` shows the call priced.
- [ ] `grep -c 'sk-ant' "$KAHYA_LOG_DIR"/*.jsonl` → 0, and the env seen by the fake worker contains only a `kahya-task-<hex32>` value in `ANTHROPIC_API_KEY` (real key never leaves kahyad); the same curl repeated with `x-api-key: wrong` from outside the worker gets `401` and a `proxy_auth_reject` ledger row.
- [ ] Governor test proves fail-closed ordering: the request that would cross 500K is blocked BEFORE forwarding (fake upstream records zero hits for it).
- [ ] With `daily_budget_usd` test-overridden to $0.01 and one priced fixture event inserted: next proxied request returns the Turkish budget-block message and ledger gains `task_paused_budget`.
- [ ] Boot with a locked/absent Keychain: `/health` still `ok`, proxied request → 503 with `Keychain erişilemiyor…`, single `keychain_unavailable` notification event.

## Out of scope
- Telegram delivery of alarms — W3-07 (hooks only here). General egress proxy/allowlist for tools — W3-05 (this proxy gates model calls only).
- Secret-lane ordering-invariant enforcement ("no byte to cloud before local classification") — W3-08 wires the classifier; this proxy is the chokepoint where it lands, so keep the `egressGate(req) error` hook shape stable for it.
- The worker's prompt-cache discipline (frozen prefix construction) — W12-09.
- The "→yerel" downgrade rung and any local model serving — W3-08 (lane) / W4-08 (rung wiring onto the routing table).
- Retry/backoff/error taxonomy for 429/5xx — W4-04 (proxy passes upstream errors through untouched for now).
- Fable-5 fallback betas — W4-04 per §4 routing.
