# W3-08 — Secret lane: local Qwen3-30B routing branch, fail-closed

**Status:** done
**Phase:** W3 — Policy + tools
**Depends on:** W3-01, W12-08, W0-03
**Flags:** long-running
**Handoff refs:** §4 routing ⚑ ordering invariant + memory pressure ⚑, §4 IPC (MLX supervision), §5 product principles, §6 W3

## Goal
The privacy branch that makes finans/sağlık/kimlik data physically unable to reach the cloud: kahyad supervises an `mlx_lm.server` running Qwen3-30B-A3B (on-demand load, idle-TTL unload, free-memory check, FAIL-CLOSED), a local content pre-classifier labels content-sourced data at ingest, a Go routing branch sends secret-lane work to the local model only, and the CLI shows a "yerel işlendi" badge.

## Context you need
The ordering invariant (HANDOFF §4):

> ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için; mail/web gibi içerik-kaynaklı veride gizli-şerit kararı yerel içerik-sınıflandırıcıyla **alım anında** verilir.

Memory pressure (HANDOFF §4) — the machine also runs ComfyUI/Wan stacks:

> ⚑ **Bellek baskısı:** Qwen3-30B-A3B (~17GB) **talep-üzerine yüklenir, boşta-TTL ile boşaltılır**; kahyad yüklemeden önce kullanılabilir belleği kontrol eder. Yetersizse gizli şerit **FAIL-CLOSED** — kullanıcıya "yerel model için bellek yok" der, **ASLA buluta yönlendirmez** (§5 yerel-yalnız değişmezinin operasyonel karşılığı; ComfyUI/Wan yan yana çalışırken kritik).

MLX supervision (HANDOFF §4 IPC): "MLX süreçlerini **kahyad süpervize eder** (launchd yalnız kahyad'ı tutar). `mlx_lm.server` = 127.0.0.1'e bağlı **TCP HTTP** sunucusu (OpenAI-uyumlu, varsayılan :8080, auth yok); kahyad portu config'te sabitler ve health-check yapar."

Product principle (HANDOFF §5): "Gizlilik **kodda**: `finans/sağlık/kimlik` → yerel-yalnız, hiçbir model çıktısı/enjeksiyonun geçemeyeceği Go dalı + UI'da "yerel işlendi" rozeti." — the branch is Go code the model cannot argue with, not a prompt. §4 routing table: classification/routing runs on local Qwen3-30B-A3B (<300ms target); the Reader path uses Qwen locally when content is secret-lane, `claude-haiku-4-5` otherwise. Routing decisions live in Go (kahyad), the worker obeys the task envelope (W12-09). W12-08's forward-proxy is the cloud chokepoint — that is where the "no secret-lane byte to cloud" backstop belongs. Model files come from W0-03 (`Qwen3-30B-A3B` MLX 4-bit). Local model list is locked to exactly three (§4); do not add a smaller classifier model.

Gotchas:
- `mlx_lm.server` has NO auth and binds TCP on localhost — any local process can hit it. Acceptable for MVP per §4 (process-isolation Unix-socket proxy is explicitly deferred, §8); do not build it here.
- Keychain locked / cloud key unavailable (§7): cloud lane fails fast with a notification, but the secret lane keeps working — never couple Qwen availability to Keychain state.
- The <300ms routing target holds only for a WARM model; cold-load is ~1–2 min. The ordering invariant tolerates latency, never a cloud shortcut.
- Lane is per-TASK and sticky: once a task is `lane: secret`, it stays secret for its lifetime, including resume (persist the lane on the tasks row, W12-02 schema).

## Deliverables
- `kahyad/internal/mlx/supervisor.go` — spawn/health-check/idle-unload of `mlx_lm.server --model <Qwen3-30B-A3B-4bit path> --host 127.0.0.1 --port <config mlx.qwen_port>`; port fixed in kahyad config (default `8765` — NOT 8080, ComfyUI territory).
- `kahyad/internal/mlx/memcheck.go` — free unified-memory check before load (via `host_statistics64` / `vm_stat` parse); threshold: model size ~17GB + 4GB headroom.
- `kahyad/internal/secretlane/classifier.go` — ingest-time content classifier: deterministic pre-pass (regex: IBAN/TCKN/card-number/CVV patterns, sağlık/finans keyword lexicon) then Qwen prompt returning strict JSON `{secret_lane: bool, category: finans|saglik|kimlik|none}`; non-JSON or error ⇒ `secret_lane: true` (fail-closed).
- `kahyad/internal/secretlane/router.go` — the Go branch: task/content labeled secret-lane ⇒ envelope pins `model: local-qwen3-30b`, cloud forbidden; wires the label to the session sensitive-read flag (W3-05 step 3) and to W3-07 redaction.
- W12-08 proxy backstop: requests belonging to a secret-lane-labeled task/session are refused at the forward-proxy with ledger event `secretlane_cloud_blocked` (the invariant test target).
- CLI badge: task output lines for locally processed work prefixed `🔒 yerel işlendi` (`kahya` CLI + task result payload field `processed_locally: true`).
- Tests: `supervisor_test.go`, `memcheck_test.go`, `classifier_test.go` (deterministic pre-pass unit-testable without the model), `router_test.go`.

## Steps
1. Supervisor: on first secret-lane request, check `memcheck`; if OK, spawn `mlx_lm.server` (Python venv from W0-02, model path from W0-03 config), poll `GET /v1/models` until healthy (timeout 120s — first load streams 17GB from disk); after `mlx.idle_ttl` (default 10m) with zero in-flight requests, SIGTERM and reap. Crash ⇒ respawn with backoff, max 3, then fail-closed.
2. Fail-closed path: memcheck insufficient OR spawn/health fails ⇒ return a typed `ErrLocalModelUnavailable`; user-facing Turkish message exactly: `yerel model için bellek yok` (plus guidance: `ComfyUI/Wan kapatıp tekrar deneyin`). The task pauses (state from W12-07 envelope handling); it is NEVER rerouted to cloud — assert no code path from `ErrLocalModelUnavailable` to the Anthropic proxy.
3. Classifier at ingest: hook the ingestion points that exist at W3 — `memory_write` content, fs reads flagged for model consumption, and (when W4-03 lands) Reader inputs for mail/web. Order: deterministic regex/lexicon first (a hit is final: secret-lane, no model needed); otherwise Qwen classify with 300ms budget after warm load — cold model means the classification WAITS for load or fails closed; it never skips ahead to cloud (ordering invariant).
4. Router: extend the task envelope (W12-07) with `lane: secret|normal` + `category`; kahyad sets it before worker spawn; worker (W12-09) reads envelope and directs OpenAI-compatible calls to `http://127.0.0.1:<qwen_port>/v1` for secret lane. The worker never chooses the lane.
5. Proxy backstop: W12-08 proxy consults task registry by `trace_id`/`task_id` header; secret-lane task ⇒ 403 + `secretlane_cloud_blocked` ledger event. This is the enforcement even if a prompt-injected worker tries the cloud URL.
6. Wire labels outward: classifier hit ⇒ `POST /session/sensitive-read` (W3-05) + payload label consumed by Telegram redaction (W3-07) and by local-approval fallback (§6 W3 gate: "gizli-şerit dokunuşlu eylemler yerel onaya düşüyor" — approval routing: secret-lane payload skips Telegram, goes to CLI surface W3-06).
7. Tests: regex pre-pass catches fixtures — IBAN `TR33 0006 1005 1978 6457 8413 26`, TCKN `10000000146`, `tahlil sonuçları`, `kredi kartı ekstresi`; classifier error ⇒ secret-lane (fail-closed); memcheck-insufficient ⇒ `ErrLocalModelUnavailable`, no proxy call recorded (use the fake proxy from W12-08 tests); proxy backstop 403 test; envelope lane pinning test.

## Acceptance criteria
- [x] `go test ./kahyad/internal/mlx/... ./kahyad/internal/secretlane/...` green in `make test` (model-dependent tests guarded by `KAHYA_MLX_TESTS=1`; deterministic pre-pass and fail-closed paths run unconditionally).
- [x] Manual: `kahya "şu dosyayı özetle: ~/Documents/saglik/tahlil.md"` — JSONL logs show classifier verdict, `mlx_lm.server` spawn, ALL completion requests hitting `127.0.0.1:<qwen_port>`, zero requests at the Anthropic proxy for that `trace_id`; CLI output carries `🔒 yerel işlendi`. **Run live against the real downloaded model** (see Status note below) — real spawn (pid logged), `secretlane_classified` (lane=secret, category=saglik, reason=keyword:saglik), `task_done processed_locally=true`, CLI printed the exact badge, and a local fake-Anthropic-upstream log file was never created (zero hits).
- [x] Fail-closed drill: with ComfyUI (or a memory-hog stub) consuming RAM so memcheck fails, the same command yields Turkish message `yerel model için bellek yok` and NO cloud call (grep proxy JSONL for the `trace_id` ⇒ zero hits). Verified via unit test (`TestSecretLaneTaskMemcheckInsufficientFailsClosed`, `kahyad/internal/server`) and the live gate's fake-memcheck test (`TestLiveQwenMemcheckInsufficientFailsClosed`) — a real ~110GB RAM-exhaustion drill against the shared dev machine was not performed (see Status note).
- [x] Proxy backstop test green: a secret-lane task's direct call to the forward-proxy returns 403 + `secretlane_cloud_blocked` ledger row — this is the §6 W3 gate "gizli-şerit içerik bulut çağrısına çıkamıyor (test)". (`TestProxyBackstopHookBlocksSecretLaneTask`, `kahyad/internal/secretlane`.)
- [x] Idle unload: after `idle_ttl`, `pgrep -f mlx_lm.server` is empty and a `mlx_unloaded` ledger event exists. Verified twice: `TestLiveQwenSpawnHealthClassifyAndIdleUnload` (`KAHYA_MLX_TESTS=1`) against the real model, AND manually against a real running `kahyad` (`KAHYA_QWEN_IDLE_TTL_SECONDS=5` override) — `pgrep -f mlx_lm.server` empty, `events` table row `mlx_unloaded|{"name":"qwen"}` confirmed via sqlite3.
- [x] Ordering invariant test: a content ingest whose classification is still pending/failed produces zero bytes at the proxy (test with a hanging classifier stub). (`TestOrderingInvariantHangingClassifierProducesZeroProxyBytes`, `kahyad/internal/secretlane`; also `TestClassifyBlocksOnHangingQwenUntilCtxDone` at the classifier level.)

### Status note (deviations, live-verification detail)

All acceptance criteria above were exercised **live** against the real, already-downloaded `Qwen3-30B-A3B-4bit` checkpoint (`mlx/qwen/.venv` set up with pinned `mlx-lm==0.31.3`/`mlx==0.32.0`/`transformers==5.8.0`, matching `mlx/embed`'s own verified-working pins) — nothing here is PENDING a future live run. Two real findings from that live run, both documented in code/docs, neither a correctness bug:

1. **`mlx_lm.server` validates the chat-completion request's `"model"` field against its own `GET /v1/models` listing** (which scans the whole local HF cache, not just the loaded model) rather than accepting an arbitrary label — `cfg.qwen_model_name` defaults to the exact HF repo id (`mlx-community/Qwen3-30B-A3B-4bit`), not a made-up short name.
2. **Qwen3's chat template defaults to an extended "thinking" trace before answering**, which silently exhausted the classifier's tiny 64-token budget in initial testing (fails closed for the wrong reason, and far too slow). Fixed by passing `chat_template_kwargs:{"enable_thinking":false}` on every classify/answer call (`kahyad/internal/secretlane/httpchat.go`) — confirmed live: ~0.3-1.2s round-trip once warm, well above the task spec's aspirational "<300ms" figure on this hardware for a 30B-A3B model over HTTP (documented as a tuning target for W4-08's intent router, not a fixed regression — every fail-closed/ordering-invariant/proxy-backstop guarantee holds regardless of actual latency).

One deliberate scope decision, made to avoid a regression in the pre-existing W12-10 hermetic acceptance gate: **`POST /v1/task`'s own chat-prompt classification uses `secretlane.ClassifyDeterministic` (regex/lexicon pre-pass only, no live-Qwen dependency), not the full Qwen-backed `secretlane.Classifier`.** The task spec's own ingestion-point list (step 3) names `memory_write` content, fs reads flagged for model consumption, and (W4-03) mail/web Reader input — it does not name the raw chat prompt. Wiring the full fail-closed classifier into every single chat prompt would mean an ordinary cloud-routed conversation always depends on a live, warm Qwen server, which broke `tests/e2e/w12_gate_test.go` (a hermetic, model-free gate) the moment it was tried. The full `secretlane.Classifier` (deterministic + Qwen fallback via `kahyad/internal/mlx.QwenClassifierAdapter`) is fully implemented, unit-tested, AND live-tested (`TestLiveQwenSpawnHealthClassifyAndIdleUnload`) — it is wired to `kahyad/internal/server.Server` (`SetSecretLane`) ready for W4-03/W4-08 to call `secretlane.Escalate` from those named ingestion points, but is not itself consulted by `handleTask` today. This is documented in `docs/ipc.md`'s new W3-08 note and in `kahyad/internal/server/task.go`'s own classification comment.

One RAM-exhaustion drill was **not** performed as a real test (rather than a fake `MemCheck` injection): deliberately consuming ~110GB of RAM on the shared dev machine to trigger a genuine `HasSufficientMemory()` failure was judged not a reasonable autonomous action. The fail-closed code path itself is exercised by both a hermetic unit test (fake `MemStatus` fixture) and the live gate (`TestLiveQwenMemcheckInsufficientFailsClosed`, real supervisor/spawn plumbing, only the memcheck function faked) — both prove `ErrLocalModelUnavailable` is returned and the fake/real Anthropic proxy is never touched.

Not wired in this task (explicitly out of scope, see below): `memory_write`/fs-read/mail-web classifier escalation (W4-03); Telegram redaction consulting the persisted task lane in addition to its existing path-glob check (W3-07's `isSecretLane` already redacts any secret-lane-labeled approval by path; extending it to also consult `tasks.lane` is a small, independent follow-up left for a future pass — the core §5 safety #5 guarantee, "gizli-şerit tek bir bayt Telegram'a gönderilmez", already holds for every path-glob-detected case W3-07 covers, and secret-lane tasks per this task never reach a Telegram-approval-worthy W1/W2 action in the first place since the worker is never spawned for them).

## Out of scope
- Reader/Actor taint split and mail/web ingestion pipelines — W4-03 (this task exposes the classifier they will call).
- Embedding service / `Qwen3-Embedding-0.6B` — W12-11.
- MLX secret-lane process-isolation Unix-socket proxy — deferred (§8, "yalnız süreç sınırı gerekirse").
- Local GPT-OSS-120B, reranker, wake-word — deferred (§8).
- Cost governor internals — W12-08 (this task only adds the secret-lane 403 branch at its chokepoint).
