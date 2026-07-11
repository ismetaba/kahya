# W12-11 — embedding pipeline (MLX) + hybrid fusion

**Status:** done
**Phase:** W1–2 — Core
**Depends on:** W12-10, W0-03
**Flags:** slidable
**Handoff refs:** §4 models ⚑ `model_ver` rule, §6 timing note

## Goal
Cross-lingual (TR→EN) retrieval works. A kahyad-supervised MLX embedding service serves `Qwen3-Embedding-0.6B` (512-dim MRL); all chunks get vectors tagged `model_ver`; KNN joins the FTS5 legs in hybrid fusion; and a full re-embed flow exists for future model upgrades.

## Context you need
Why this may run after the W1–2 gate (HANDOFF §6 ⚑, verbatim):

> ⚑ **Zamanlama notu:** W1–2 kapsamı yüklüdür; **W1–2 kabulü FTS5-only aramayla sağlanır** — embedding hattı (MLX süreç + Qwen3-Embedding-0.6B servisi + chunk gömme) ayrı iş kalemidir, sığmazsa W3–4'e kayar (şemadaki embedding kolonları + `model_ver` yine gün 1'de açılır; çok-dilli TR→EN retrieval embedding hattı bitene dek çalışmaz).

The binding version rule (HANDOFF §4 ⚑, verbatim):

> ⚑ **`model_ver` kullanım kuralı:** her vektör satırı `model_ver` taşır; KNN sorguları daima **tek aktif `model_ver`'e filtrelenir** (karışık-versiyon KNN yasak). Gömülü yükseltme = Markdown kaynak-gerçeğinden **tam yeniden-gömme**; §5-Hafıza-#5 retrieval eval kapısı yeşilse aktif versiyon değişir, eski vektörler sonra silinir.

Supervision contract (HANDOFF §4 ⚑, verbatim):

> - MLX süreçlerini **kahyad süpervize eder** (launchd yalnız kahyad'ı tutar). `mlx_lm.server` = 127.0.0.1'e bağlı **TCP HTTP** sunucusu (OpenAI-uyumlu, varsayılan :8080, auth yok); kahyad portu config'te sabitler ve health-check yapar. `mlx-whisper` bir sunucu değil, worker içinde **kütüphane** olarak.

Model identity (HANDOFF §4): "Yerel filo v1'de **tam üç** yerleşik model: `whisper-large-v3-turbo`, `Qwen3-Embedding-0.6B` (512-dim MRL, `model_ver` etiketli), `Qwen3-30B-A3B`." — this task touches ONLY the embedding model (~1.2GB; no §4 memory-pressure gating needed — that ⚑ is about the 30B and lands with W3-08, but reuse-friendly supervisor code helps).

Implementation note: `mlx_lm.server` has no `/v1/embeddings` endpoint, so the embedding helper is a small Python HTTP service of our own that follows the SAME supervision contract as the quote above (127.0.0.1, fixed config port `embed_port` 8092, OpenAI-compatible, no auth, kahyad-supervised). Follow the Qwen3-Embedding model card: last-token pooling, L2-normalize, then truncate to 512 dims (MRL) and re-normalize.

Prior output: W12-03's `chunk_vec` vec0 table (512-dim, cosine, `model_ver` column) + fusion scaffold; W12-04 indexer; W0-03 downloaded the model into the HF cache. Config `active_embed_model_ver` = `qwen3-embedding-0.6b:512:v1` (W12-01).

## Deliverables
- `mlx/embed/server.py` + `mlx/embed/requirements.txt` (pinned) + `mlx/embed/README.md` — the service; own venv `mlx/embed/.venv` (separate process, separate deps from `worker/` per §4's three-process architecture).
- `mlx/embed/test_server.py` — pytest (skips unless model present).
- `kahyad/internal/mlxsup/supervisor.go` + `supervisor_test.go` — generic child-process supervisor (spawn, health poll, restart w/ backoff, stop-on-shutdown) — W3-08 reuses this for the 30B.
- `kahyad/internal/embed/client.go` + `backfill.go` + tests — embed client, chunk backfill, re-embed flow.
- Search integration: KNN leg + 3-way fusion in `kahyad/internal/search`.
- CLI: `kahya reindex --re-embed` flag (maps to `/v1/reindex {"full":false,"re_embed":true}`).

## Steps
1. `mlx/embed/server.py`: `POST /v1/embeddings` `{"model":"qwen3-embedding-0.6b","input":["…",…]}` → OpenAI-shaped response with 512-float vectors; `GET /health` → `{"status":"ok","model_ver":"qwen3-embedding-0.6b:512:v1"}`. Bind `127.0.0.1:$KAHYA_EMBED_PORT` only. Batch ≤64 inputs per request; deterministic output for identical input (temperature-free — embeddings are deterministic by nature; assert in test).
2. Supervisor: start command from config (`embed_cmd`, default `["mlx/embed/.venv/bin/python","mlx/embed/server.py"]`); poll `/health` (2s interval, 60s startup grace — first model load is slow); restart on crash with exponential backoff (1s→60s cap); kill on kahyad shutdown (same process-group pattern as W12-07). Lazy start: spawn on first embedding need, not at boot. Supervisor state surfaces in kahyad `/health` as `"embed":"ok|starting|down|disabled"`.
3. Embed client + backfill: after every reindex (and on a `re_embed` trigger), select chunks lacking a `chunk_vec` row with `model_ver = cfg.active_embed_model_ver`; batch 32; `INSERT` vectors with the active `model_ver`. Ledger event `embed_backfill` `{chunks, model_ver, duration_ms}`. Failures leave chunks vector-less (FTS still serves them — degraded, never broken); retry on next reindex.
4. KNN leg in `search.Search`: embed the query (1 call), then `SELECT chunk_id, distance FROM chunk_vec WHERE embedding MATCH ? AND k = ? AND model_ver = ?` — **always** bound to the single active `model_ver` (⚑ rule above; make it impossible to call the query without the filter — no variadic/default-off API). Score = `1 - distance` (cosine), min-max normalized like the FTS legs. New fusion weights (config): `tri 0.3, uni 0.2, vec 0.5`; if the embed service is down or the query embed call fails, fall back to W12-03's FTS-only weights and log `event=search_degraded_no_vec` (warn) — search never hard-fails on the vector leg.
5. Re-embed flow (`re_embed:true`): re-embeds ALL chunks from the current index (which is itself rebuilt from markdown — "Markdown kaynak-gerçeğinden tam yeniden-gömme") under `cfg.active_embed_model_ver`, then deletes `chunk_vec` rows where `model_ver != active`. Version SWITCH procedure (documented in `mlx/embed/README.md`, executed rarely): set new `active_embed_model_ver` in config → restart → `kahya reindex --re-embed`. The §5-Memory-#5 eval gate before switching becomes enforceable when W78-01 exists; until then the README documents it as a mandatory manual step (do not build the gate here).
6. Tests: supervisor (fake child script: healthy/crashing/slow-start paths); client against a stub embed server (Go httptest); **mixed-version exclusion** — insert a vector under `model_ver='old:v0'` for a fixture chunk, assert KNN never returns it and re-embed deletes it; degraded-fallback path (stub down → FTS-only results + warn log); `mlx/embed` pytest — 512 dims, L2 norm ≈ 1.0, deterministic repeat, Turkish input `Kadıköy'de ev fiyatları` embeds without error.
7. Cross-lingual verification (requires real model; mark test with build tag `mlx` + `make test-mlx`): index two fixture notes — EN: `The gold-token backend uses NATS JetStream for saga orchestration.` and an unrelated TR decoy — query `altın projesinde saga nasıl kurulmuştu?` must rank the EN note first via the vec leg (FTS legs alone fail this; assert the vec leg contributed by also asserting FTS-only search misses it).

## Acceptance criteria
- [x] `make test` green (all non-`mlx` tests, incl. mixed-version exclusion and degraded fallback); `make test-mlx` green on this machine with the downloaded model. Verified repeatedly; `make test-mlx` includes the real `kahyad/internal/mlxe2e` cross-lingual gate (~37s) + `mlx/embed`'s pytest (8 tests, real model).
- [x] `curl -s 127.0.0.1:8092/health | jq -r .model_ver` → `qwen3-embedding-0.6b:512:v1` after triggering a search (lazy start observed in logs: `event=mlx_spawn`). Verified against a real `make install-agent`-launched kahyad: `mlx_spawn` logged on the first `/v1/reindex`'s backfill, then `curl 127.0.0.1:8092/health` returned exactly `{"status":"ok","model_ver":"qwen3-embedding-0.6b:512:v1"}`.
- [x] After `bin/kahya reindex --re-embed` on the real corpus: `sqlite3 brain.db "SELECT count(*) FROM chunks;"` equals `sqlite3 brain.db "SELECT count(*) FROM chunk_vec WHERE model_ver='qwen3-embedding-0.6b:512:v1';"` and `SELECT count(DISTINCT model_ver) FROM chunk_vec;` → 1. Verified on the real `~/Kahya/memory` corpus (14 files, 117 chunks): both counts 117, distinct model_ver count 1. (sqlite3 CLI itself can't load the CGo-only vec0 module, so counts were read via a Go program using `kahyad/internal/store.Open` - the same code path kahyad itself uses.)
- [x] Cross-lingual live check: retrieves the (English) `gold-token-backend.md` note into the `<hafiza>` block for `"altın projesinde saga desenini nasıl kurmuştuk?"` (ranked FIRST, score 0.8) — verified via the `hafiza_injected` ledger payload (`block_sha256` present, `gold-token-backend.md#0` first in `block`). Done via `POST /v1/memory/search {"for_injection":true}` directly rather than literally `bin/kahya "..."`, since this sandbox has no real Anthropic credentials to drive `bin/kahya`'s full `/v1/task` agent pipeline (out of this task's scope) - `/v1/memory/search` is the exact same retrieval + injection + ledger code path `UserPromptSubmit` calls.
- [x] Kill the embed service process: kahyad log shows restart with backoff (`event=mlx_exit` → `mlx_restart_scheduled` backoff_ms=1000 → `mlx_spawn`); during downtime a search still returns FTS results (`gold-token-backend.md` still top hit) with `event=search_degraded_no_vec`. Verified against the real install; embed service also auto-recovered (`/health` "embed" back to "ok") within one backoff cycle.
- [x] `pgrep -f 'mlx/embed'` after `launchctl bootout`/kahyad shutdown → empty (supervisor cleans up; launchd holds only kahyad, per §4 ⚑). Verified both via `launchctl bootout` (real `make install-agent` LaunchAgent) and a plain `SIGTERM` to a manually-run `bin/kahyad` - both leave `pgrep -f 'mlx/embed'` empty.

## Out of scope
- Qwen3-30B-A3B secret-lane serving, memory-pressure fail-closed gating, ingest classifier — W3-08 (it reuses `mlxsup`).
- `mlx-whisper` — W6-02 (library in worker, not a server).
- Reranker (§8: "yalnız eval precision düşerse"); retrieval QA eval + the version-switch gate automation — W78-01.
- Embedding anything other than chunks (facts/profile cards) — v2.
- Changing FTS behavior or the W12-10 gate — that gate stays FTS-only-passable per §6 ⚑.
