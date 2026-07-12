# W4-04 — Cloud-call error taxonomy, backoff, bekliyor-yeniden-deneme

**Status:** done
**Phase:** W4 — Durability
**Depends on:** W12-08, W4-02, W12-09 (worker `cloud_unreachable` event), W3-08 (secret-lane marker header, consumed)
**Flags:** none
**Handoff refs:** §6 W4, §4 routing (Fable 5 rule), §4 IPC ⚑ (forward-proxy)

## Goal

Cloud model calls get a deterministic error taxonomy: retryable failures (429/5xx/network) are
retried with exponential backoff at the proxy, then park the task in the
`bekliyor-yeniden-deneme` state for outbox-driven redelivery; non-retryable failures surface a
clean Turkish error; `claude-fable-5` requests always carry the server-side-fallback beta with
an Opus fallback. Offline commands finish when the network returns or fail explicitly — half of
the W4-07 gate.

## Context you need

Binding text (HANDOFF, quote verbatim):

§6 W4:
> **bulut çağrı hata taksonomisi** (retryable 429/5xx/ağ + üstel backoff + max deneme + `bekliyor-yeniden-deneme` durumu)

§4 routing:
> Fable 5 **asla varsayılan değil** ve daima `betas:["server-side-fallback-2026-06-01"]` + `fallbacks:[{model:"claude-opus-4-8"}]` ile (30-gün saklama zorunluluğu + güvenlik-bitişik işte red sınıflandırıcıları).

- All worker cloud traffic already flows through the W12-08 localhost forward-proxy
  (`ANTHROPIC_BASE_URL=http://127.0.0.1:<port>`), which is also where the cost governor and
  cache-hit metric live — so the proxy is the single choke point to classify at.
- W4-02 created the task states (including `bekliyor-yeniden-deneme`), `next_retry_at`,
  `attempts`, and the outbox dispatcher that re-spawns workers with `resume`+`session_id`.
- Fail-closed convention (tasks/README.md): secret-lane work NEVER falls back to cloud — this
  task touches only the cloud lane; local-lane failures are W3-08's domain and must not gain a
  cloud fallback here.
- Model IDs come from HANDOFF §9 only: `claude-opus-4-8`, `claude-sonnet-5`, `claude-haiku-4-5`,
  `claude-fable-5`.

## Deliverables

- `kahyad/internal/cloudretry/taxonomy.go` + `taxonomy_test.go` — classification
- `kahyad/internal/cloudretry/backoff.go` + `backoff_test.go` — jittered exponential backoff
- Proxy integration in the W12-08 package (inline retries + Fable-5 request shaping)
- Task-level retry scheduling in `kahyad/internal/task/` (W4-02 package)
- Turkish user-facing strings (exact, step 5) in the notification path
- Config keys: `cloud.retry.max_inline`, `cloud.retry.task_schedule`, `cloud.retry.give_up_after`

## Steps

1. Taxonomy (`Classify(status int, err error) Class`):
   - `Retryable`: HTTP 408, 429, 500, 502, 503, 504, 529 (Anthropic overloaded), and transport
     errors (DNS failure, connection refused/reset, TLS handshake timeout, `context.DeadlineExceeded`).
   - `NonRetryable`: 400, 401, 403, 404, 413, 422 (and any other 4xx not listed above).
   - Honor a `retry-after` header when present (cap 60s); otherwise exponential backoff.
2. Inline proxy retries: on `Retryable`, retry the upstream request up to
   `max_inline` (default 3) with backoff 1s→2s→4s ± 20% jitter. Requests with non-replayable
   streamed request bodies are buffered by the proxy (it already reads bodies for the token
   ceiling — reuse that buffer). Each retry logs JSONL with `trace_id`, attempt number, status.
3. Exhaustion → task parking: when inline retries are exhausted, the proxy returns a typed
   error to the worker; the worker (W12-09 harness) emits `{"event":"cloud_unreachable"}` and
   exits non-zero; kahyad transitions the task to `bekliyor-yeniden-deneme`, sets
   `next_retry_at` per `task_schedule` (default: 1m, 5m, 15m, 60m, then hourly), increments
   `attempts`, appends ledger event `task.waiting_retry`. The W4-02 outbox dispatcher picks it
   up at `next_retry_at` and resumes the session — no new mechanism here.
4. Give-up: after `give_up_after` (default 24h) in retry, transition to `failed` + notify.
5. Turkish strings (exact bytes, used verbatim in notifications and tests):
   - parked: `Bulut servisine ulaşılamıyor; görev bekliyor-yeniden-deneme durumunda. Ağ dönünce otomatik devam edecek.`
   - non-retryable: `Bulut çağrısı kalıcı hatayla reddedildi (<sebep>). Görev durduruldu.`
   - give-up: `Yeniden deneme süresi doldu (24 sa). Görev kapatıldı: <özet>.`
6. Non-retryable path: task → `failed` immediately, notification uses the non-retryable string
   with `<sebep>` = short English API error id (technical output stays English per §3 language
   policy), ledger event `task.failed` with status code.
7. Fable-5 shaping at the proxy: for any request body with `"model":"claude-fable-5"`, ensure
   `betas` contains `"server-side-fallback-2026-06-01"` and `fallbacks` equals
   `[{"model":"claude-opus-4-8"}]`; inject them if absent and append ledger event
   `proxy.fable5_shaped`. Never do the reverse (no request is upgraded to Fable-5; routing is
   decided in Go per §4).
8. Tests (httptest fake upstream, all in `make test`):
   - Table-driven taxonomy test covering every status above + wrapped `net.OpError`.
   - 429×2 then 200 ⇒ one logical success, exactly 3 upstream hits, backoff delays observed
     (injectable clock), cost-governor counters counted once.
   - Always-503 ⇒ worker receives typed error; task row reaches `bekliyor-yeniden-deneme` with
     `next_retry_at` ≈ now+1m; dispatcher (fake clock) retries; upstream healed ⇒ task `done`.
   - Always-503 past `give_up_after` ⇒ `failed` + give-up string in the notification event.
   - 401 ⇒ zero retries, `failed`, non-retryable string present.
   - Fable-5 request without betas/fallbacks ⇒ upstream sees both injected;
     `proxy.fable5_shaped` event exists.
   - Retry-after honored: 429 with `retry-after: 2` ⇒ second attempt ≥2s later (fake clock).

## Acceptance criteria

- [x] `make test` green including all step-8 tests.
- [~] Offline smoke (scripted, reused by W4-07): the underlying mechanism is proven by
      `kahyad/internal/outbox`'s `TestDispatcherRetriesParkedTaskAfterCloudHeals` (a task parked
      in `bekliyor-yeniden-deneme` is picked up by the real outbox dispatcher once due and
      completes once the upstream heals) + `kahyad/internal/task`'s `TestParkOrGiveUp*` tests
      (exact `next_retry_at`/notification string). A literal `kahya "..."` CLI-driven shell
      script against a blackholed `127.0.0.1:1` upstream is deliberately NOT added here — that
      end-to-end script is explicitly W4-07's own deliverable (Out of scope, below); this task
      makes the mechanism it will drive real and independently tested.
- [x] JSONL proxy log for the smoke run shows attempts 1..3 with the same `trace_id` as the
      task's ledger events — proven directly by
      `anthproxy.TestRetryAttemptsLoggedAsJSONLWithTraceID` (reads the real JSONL file back).
- [x] Behavioral test in `make test`: a request carrying the W3-08 secret-lane marker sent to
      the proxy is rejected (4xx, ledger event) with **zero** fake-upstream hits — including on
      the retry path (marker + 503 must not retry into the cloud either). Fail-closed, never
      fallback; no retry/backoff branch added in this task weakens it
      (`TestSecretLaneTaskNeverRetriesIntoCloud`). Deviation: the real W3-08 mechanism
      (`kahyad/internal/secretlane.NewProxyBackstopHook`) is a server-side, per-task lane lookup
      keyed on `task_id` — not a client-suppliable "marker header" (a header would be
      worker-controllable and therefore weaker) — so the ledger event asserted is the real,
      already-shipped `secretlane_cloud_blocked` (`secretlane.EventSecretLaneCloudBlocked`), not
      a new `proxy.secret_lane_blocked` constant; introducing a second, differently-named event
      for the identical fail-closed check was judged riskier than reusing the real one.
- [x] Fable-5 shaping verified against a recorded request body in the fake-upstream test
      (`TestFable5ShapingInjectsBetaAndFallback`).

## Out of scope

- Budget/ceiling logic, cache-hit metrics, Telegram alarm wiring — W12-08/W3-07 own these; this
  task only adds retry/shaping to the same proxy.
- Tool-side-effect retry safety — W4-02 receipts own that; this task never re-executes tools,
  it only re-delivers tasks.
- Local (MLX) model errors, memory-pressure handling, secret-lane routing — W3-08 (§4 memory
  pressure ⚑); explicitly no cloud fallback may be added for them here.
- The full W4-07 end-to-end gate run.
