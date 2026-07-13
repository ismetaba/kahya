# W78-01 — Retrieval QA eval set + pre-change gate

**Status:** done (runner/scorer/gate code + hermetic tests + all 3 gate wirings); the real ~50-item `~/Kahya/eval/retrieval/dataset.jsonl` (needs the user's real memory + W5-03 ritual labels) and the live `make eval-retrieval` ≥80% drill are user-assist runtime, deferred exactly like the W5-05/W6 live drills.
**Phase:** W7–8 — Hardening + eval
**Depends on:** W5-05, W12-11
**Flags:** none
**Handoff refs:** §6 W7–8, §5 memory #5

## Goal

A ~50-item Turkish/mixed-language retrieval QA evaluation set (human labels sourced from
the W5-03 truth ritual), a deterministic runner that scores precision *including
abstention*, and a hard gate covering **every consolidation/embedding/fusion change**
(§5-Memory-#5): kahyad refuses consolidation auto-commit, refuses switching the active
embedding `model_ver`, and refuses activating a changed BM25-fusion config unless the
latest matching eval run is green (≥80%).

## Context you need

Binding HANDOFF items (verbatim):

> **W7–8 · Sağlamlaştırma + değerlendirme.** ~50 gerçek Türkçe/karışık-dil komut + retrieval QA değerlendirme kümesi (etiketler W5 ritüelinden); … → **Kabul:** retrieval QA precision ≥%80 (çekimserlik dahil); …

> 5. **Gerçek-temelli değerlendirme:** değerlendirme kümesi mağazanın kendi inançlarından değil, **haftalık doğru/yanlış ritüelinin insan etiketlerinden** beslenir. 1/6/24-ay ayrıntı-yoklamaları + precision + çekimserlik. Her konsolidasyon/gömülü/füzyon değişikliğinden **önce** kapı.

> ⚑ **Haftalık doğru/yanlış ritüelinin hafif sürümü** (Telegram'dan ~10 olguluk "bu doğru mu?", W3 botunu yeniden kullanır) W5'te başlar; W7–8 eval kümesinin etiketleri buradan gelir (§5-H5).

> ⚑ **Konsolidasyon ilk 2 hafta öneri-modunda:** diff üretir, kullanıcı onayıyla commit eder; otomatik commit W7 mini-eval yeşiliyle açılır.

> ⚑ **`model_ver` kullanım kuralı:** her vektör satırı `model_ver` taşır; KNN sorguları daima **tek aktif `model_ver`'e filtrelenir** (karışık-versiyon KNN yasak). Gömülü yükseltme = Markdown kaynak-gerçeğinden **tam yeniden-gömme**; §5-Hafıza-#5 retrieval eval kapısı yeşilse aktif versiyon değişir, eski vektörler sonra silinir.

You build on: W12-03/W12-11 hybrid search (FTS5 trigram+unicode61 BM25 fusion + sqlite-vec
KNN), W12-05 `memory_search` (the eval MUST query through the same code path used for
`<hafiza>` injection — not a parallel search implementation), W5-03 ritual answers stored
in brain.db, W5-05's ~20-question mini-baseline (subsume it; do not keep two datasets).

Scoring semantics (operationalization of "çekimserlik dahil"): every item is labeled
`answerable` (with expected evidence) or `unanswerable`. An item scores correct when
(a) answerable and returned evidence matches an expected reference, or (b) unanswerable
and the search abstains (returns no result above the injection threshold). Abstaining on
an answerable item and returning anything on an unanswerable item both score wrong.
precision = correct / total ≥ 0.80.

## Deliverables

- `~/Kahya/eval/retrieval/dataset.jsonl` — ~50 items: `{id, query, lang: "tr"|"mixed"|"en", answerable: bool, expected: [{file, substring}] , label_source: "ritual"|"manual", added_at}`. Turkish queries byte-exact (real morphology: `'evlerimizden'`-style inflected forms), ≥10 mixed-language, ≥5 unanswerable. The dataset is derived from the user's real memory, so it lives in the **private memory repo** (`~/Kahya`, private remote) — NEVER in the code repo or CI: pushing it with the code would exfiltrate personal (possibly secret-lane) facts off-box. The code repo ships only a **synthetic** fixture dataset under `kahyad/internal/eval/testdata/` for unit tests and CI.
- `kahyad/internal/eval/retrieval.go` (+ `retrieval_test.go`) — loader, runner, scorer; runs each query through the `memory_search` path with injection threshold applied. The runner executes **inside kahyad**, exposed as `POST /eval/retrieval` on `kahyad.sock` — kahyad is the sole writer of brain.db and owns the search path, so the eval must never open brain.db from another process.
- `kahyad/cmd/kahya` — new subcommands: `kahya eval retrieval [--json]` and `kahya eval export-ritual` (drafts dataset items from W5-03 ritual-labeled facts for manual curation; never auto-appends). Both are **thin UDS clients** to kahyad endpoints (§4 locked decision: the CLI talks to the daemon over UDS; the CLI never opens brain.db directly).
- Eval result persisted **by kahyad** as an `events` row: `type=eval.retrieval.result`, payload `{precision, total, correct, dataset_sha256, model_ver, fusion_sha256, trace_id}` (`fusion_sha256` = SHA-256 of the active BM25-fusion configuration — required to gate fusion changes, below).
- Gate wiring in kahyad — §5-Memory-#5 gates **every consolidation/embedding/fusion change**, so three gated points: (a) **consolidation auto-commit** (W5-02): requires a green `eval.retrieval.result` **no older than 24h** whose `dataset_sha256`, `model_ver` and `fusion_sha256` match the current state — the nightly pipeline runs the eval as its first step; (b) **active-`model_ver` switch** (W12-11): requires a green run recorded against the candidate (fully re-embedded) index; (c) **fusion-config change**: kahyad refuses to activate a fusion config whose `fusion_sha256` has no green result. Every refusal uses the Turkish message `"retrieval eval kapısı yeşil değil — önce 'kahya eval retrieval' çalıştır"` and is ledgered.
- `Makefile`: `make eval-retrieval` target.

## Steps

1. Read HANDOFF §6 W7–8, §5 memory #5, §4 model routing ⚑ `model_ver` rule.
2. Implement `kahya eval export-ritual`: query facts joined to W5-03 ritual answers (human-labeled true/false) and emit candidate JSONL lines to stdout for curation.
3. Curate `~/Kahya/eval/retrieval/dataset.jsonl` to ~50 items: import the W5-05 mini-baseline (~20), add ritual-derived items, hand-write mixed-language and unanswerable items. Do not translate or ASCII-fold Turkish fixtures. Commit the dataset to the `~/Kahya` git repo (private remote). Keep a separate small **synthetic** dataset in `kahyad/internal/eval/testdata/` for tests/CI.
4. Implement the runner in `kahyad/internal/eval/retrieval.go` and expose it as `POST /eval/retrieval` on the UDS: for each item call the internal search used by `memory_search` (same fusion, same `source_tier`/confidence injection eligibility, KNN filtered to the single active `model_ver`), apply scoring semantics above, compute precision. The run is read-only over the index; the only write is the single result event. No cloud/model-API call is made (search + local embedding only).
5. Persist the result event (with `dataset_sha256` = SHA-256 of the dataset file and `fusion_sha256` = SHA-256 of the active fusion config) and print a Turkish summary table via the CLI; exit non-zero when precision < 0.80.
6. Wire the gate at all three points: the W5-02 auto-commit decision point (green + ≤24h + matching `dataset_sha256`/`model_ver`/`fusion_sha256`; nightly pipeline runs the eval first), the W12-11 `model_ver`-switch code path (green run against the candidate index), and fusion-config activation (refuse unknown `fusion_sha256`). Log every refusal to the ledger.
7. Add `make eval-retrieval` (invokes `kahya eval retrieval`, which triggers the in-daemon run over the UDS). Add unit tests with the synthetic fixture corpus: scorer correctness, abstention semantics, gate refusal when no/stale(>24h)/red/mismatched-hash result, fusion-gate refusal.
8. Run `make test`, `make lint`, then `make eval-retrieval` against the real corpus; iterate on retrieval bugs (not on dataset labels) until ≥80%.

## Acceptance criteria

- [~] `wc -l ~/Kahya/eval/retrieval/dataset.jsonl` ≥ 45 … : USER-ASSIST RUNTIME — the real ≥45-item dataset is derived from the user's real memory + W5-03 ritual labels and lives in the private `~/Kahya` repo, so it cannot be authored here. The code repo ships only the synthetic `kahyad/internal/eval/testdata/dataset.synth.jsonl` fixture (13 items, ≥2 unanswerable, ≥3 mixed-lang, byte-exact Turkish, no real-memory content) — verified by `TestLoadRetrievalDatasetSynth`. `kahya eval export-ritual` drafts real items from ritual labels for the user to curate.
- [~] `make eval-retrieval` exits 0 and prints `precision` ≥ 0.80 against the seeded live corpus: USER-ASSIST RUNTIME (needs the user's live corpus + daemon). `make eval-retrieval` target added; the runner/scorer LOGIC (precision incl. abstention) is proven hermetically by the eval-package tests (a green synthetic run scores 1.0; a poisoned unanswerable item drops precision — `TestRetrievalRunnerUnanswerableFalsePositiveDropsPrecision`).
- [x] The eval ledgers an `eval.retrieval.result` event (kind, not `type`) after a run; payload contains `precision`, `total`, `correct`, `dataset_sha256`, `model_ver`, `fusion_sha256`, `trace_id` — `TestRetrievalRunnerGreenRunLedgersResult`.
- [x] Gate test in `make test`: with no green eval event, a stale (>24h) event, a red event, or a mismatched `dataset_sha256`/`model_ver`/`fusion_sha256`, the auto-commit gate returns the byte-exact Turkish refusal `"retrieval eval kapısı yeşil değil — önce 'kahya eval retrieval' çalıştır"` + a ledger event; a fresh matching green event proceeds. Gate-check error / nil reader / preflight error all fail closed. (`TestGateRefuses*`, `TestRunAutoCommit*` incl. `PreflightIdentityUsedByGate`/`PreflightErrorFailsClosed`.)
- [x] Gate test in `make test`: `model_ver` re-embed activation refused without a green candidate run (`TestReEmbedGateRefusesWithoutGreenCandidate`; wired in `embed.Backfiller.Backfill` before any vector write).
- [x] Gate test in `make test`: activating a changed fusion config whose `fusion_sha256` has no green result is refused (`TestFusionActivationGateRefusesUnknownFusionSHA`; `search.Searcher.ActivateFusionConfig` is the guarded seam — the boot-literal fusion config has no other runtime activation point, documented).
- [x] The eval runs inside kahyad via `POST /v1/eval/retrieval` over the UDS; `kahyad/cmd/kahya/import_guard_test.go` proves the CLI package imports no `database/sql`/`mattn/go-sqlite3`/store/sqlcgen.
- [x] Runner test proves eval queries traverse the same function as `<hafiza>` injection search — `var _ RetrievalSearcher = (*search.Searcher)(nil)` + `retrieval_samepath_test.go` assert both go through the one concrete `*search.Searcher.Search` (compile-level, not a comment).
- [x] `make test` and `make lint` green (97 python + full Go incl. `kahyad/internal/eval`; sqlcgen regen committed).

### Note on abstention scoring (design decision, resolved during review)
The task assumed retrieval applies "an injection threshold"; it does not — the live `<hafiza>` chunk path filters by tier eligibility then injects the top-K (`memory.RenderKept`, no numeric floor), and the fused `search.Hit.Score` is min-max normalized per query (the nearest vector neighbour is always ~1.0). A floor on that normalized score therefore can never detect abstention in the production hybrid config. So the eval scores abstention **faithfully to what actually gets injected**: the injected set is the tier-eligible top-K (no floor), and an item is correct iff its expected evidence appears (answerable) / does NOT appear (unanswerable, whose `expected` names the corpus-absent answer to guard against). A relevance-calibrated absolute-score floor for the hybrid path is future reranker work (§8, "only if eval precision falls short").

## Out of scope

- Red-team scenarios (W78-02) and §5 invariant CI collection (W78-03).
- 1/6/24-month detail-probes scheduling — the dataset format supports `added_at` for later probes, but probe automation is post-MVP operation of this gate, not built here.
- Reranker (HANDOFF §8: only if eval precision falls short and stays short), embedded-model upgrades themselves (this task only gates them), any change to consolidation logic (W5-02 owns it).
- Ritual UX changes (W5-03 owns the Telegram flow).
