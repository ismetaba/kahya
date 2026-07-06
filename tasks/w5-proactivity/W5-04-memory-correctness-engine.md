# W5-04 — Memory correctness engine

**Status:** todo
**Phase:** W5 — Proactivity + consolidation
**Depends on:** W5-02
**Flags:** none
**Handoff refs:** §5 memory #1–#4 ⚑

## Goal

kahyad gains a single fact-write path (the "correctness engine") through which every fact insert/update flows — from consolidation detail-atom promotion (W5-02), `memory_write` (W12-05), and the truth ritual (W5-03) — enforcing the §5 memory rules: source-trust lattice with agent-derived quarantine, log-odds confidence with negative evidence and retraction, evidence-gated entity merge/split, and evidentiality from Turkish -mış morphology.

## Context you need

HANDOFF §5 memory #1–#4 verbatim — these are the spec; implement them literally:

> 1. **Kaynak-güven kafesi:** her olgu `source_tier` ∈ {`user_edit`(1.0) › `user_asserted`(≤.95) › `external_doc`(≤.8) › `screen`(≤.7) › `agent_derived`(≤.4)}. Ajan-türevi karantinada, kullanıcı onaylayana dek profil kartından/enjeksiyondan hariç.
> 2. **Bölünebilir, kanıt-kapılı varlık birleştirme:** isim benzerliğiyle asla oto-birleştirme (Türkçe'de sayısız Emre/Ahmet). En az bir ayırt edici kanıt şart. Merge-defteri + **varlık-bölme** operasyonu. Şüpheli aynı-isim → yeni geçici varlık.
> 3. **Negatif kanıt + log-odds güven:** noisy-OR ratchet yok; aynı-oturum tekrarı tek kanıt sayılır. Kullanıcı reddi/başarısız tazelik-yoklaması güveni düşürür; <0.3 enjeksiyondan çıkar. "Artık sevmiyorum" → geri-çekme.
> 4. **90+ gün sıcak pencere + ayrıntı-atomu:** 48 saat değil ≥90 gün. Soğutmadan önce sayı/tarih/alıntı/karar/söz'ler yapılandırılmış olgulara terfi. Her özet **ham kanıttan** üretilir, asla alt-özetten.

- Schema already exists (W12-02, from the §5 ⚑ schema block): `facts (subject, predicate, object, source_tier, evidentiality [witnessed|reported|inferred], confidence [log-odds], importance, valid_from/to, status, evidence, extractor_ver)`, `entities + entity_aliases`, `evidence (fact_id, episode_id, session_id, polarity ±)`, `merge_ledger`. Do **not** add new fact columns; add migrations only if a supporting table or column is genuinely missing (e.g. a goose migration adding `entities.provisional` and the user-confirmation marker if W12-02 did not create them — check the actual schema first).
- Extraction vs. enforcement: LLM extractors (consolidation session, W12-09 sessions) only *propose* candidate facts as structs (subject/predicate/object/evidentiality/quote spans). The engine validates them Go-side (schema, length+charclass limits per §5 safety #2) and applies rules 1–3. Routing per §4: extraction of non-secret-lane content = `claude-haiku-4-5`; secret-lane content = local `Qwen3-30B-A3B` only, fail-closed (never cloud). Every candidate carries `extractor_ver`.
- **`source_tier` is assigned by the engine from Go-side provenance, NEVER taken from model output.** §5 product principles ("Güvenlik **yürütücüde**… Modeli anahtarları olmayan parlak-ama-fazla-özgüvenli bir junior gibi ele al") and §5 safety #3 ("Güvenilmez-alınmış hafıza profil kartına giremez") bind here: a prompt-injected extractor must not be able to mint a high-trust fact by claiming `source_tier=user_asserted` in its struct. Canonical assignment, decided by the kahyad call-site + W4-03 taint record for the originating `session_id`:
  - `user_edit` — ONLY derived from git author on `~/Kahya/memory` commits (W12-04 indexer path); no runtime candidate can carry it.
  - `user_asserted` — ONLY when kahyad itself marks the candidate as a direct user utterance from a session whose W4-03 taint record is clean (missing taint record ⇒ untrusted ⇒ NOT user_asserted, fail-closed). A W5-03 ritual answer is user input at this weight.
  - `external_doc` / `screen` — set by the ingest call-site from the source type of the parsed content.
  - `agent_derived` — everything an LLM extractor proposed on its own (consolidation promotion, W12-09 sessions). Any tier field present in an extractor's candidate struct is ignored and clamped to `agent_derived`; the clamping is ledgered.
- Log-odds mechanics (canonical constants, tunable, defined once in code): tier evidence weights as log-odds deltas capped at the tier's max probability from rule #1 — `user_edit` cap 1.0, `user_asserted` +2.94 (cap p=.95), `external_doc` +1.39 (cap .8), `screen` +0.85 (cap .7), `agent_derived` +? capped at .4 (−0.405); user denial −2.94. Injection threshold p=0.3 ⇒ log-odds −0.847. No noisy-OR: evidence sums, per-session deduped, tier cap clamps.
- Retraction: user statements matching retraction semantics ("Artık sevmiyorum", "artık ... değil", explicit corrections) close the fact: set `valid_to=now`, `status=retracted`, negative evidence row — never delete (two-temporal columns exist for this; full graph queries are v2, §8).
- Evidentiality: reported speech via -mış/-miş/-muş/-müş morphology ⇒ `reported`; direct assertion/observation ⇒ `witnessed`; engine-inferred ⇒ `inferred`. The extractor sets it; the engine validates the enum and defaults to `inferred` when absent (fail-conservative).
- kahyad is the sole brain.db writer; the engine is a kahyad-internal package, invoked by W12-05 `memory_write`, W5-02 promotion, and W5-03 answers (refactor W5-03's minimal update onto this engine if it landed first).

## Deliverables

- `kahyad/internal/factengine/engine.go` — `WriteFact(candidate) (FactID, error)`: validation, lattice caps, quarantine flag, log-odds update, same-session evidence dedupe on `(fact_id, session_id, polarity)`.
- `kahyad/internal/factengine/retract.go` — retraction detection on user-asserted candidates + fact closure.
- `kahyad/internal/factengine/entity.go` — entity resolution: alias lookup via `entity_aliases`; merge **only** with ≥1 distinguishing evidence (shared unique attribute: employer+role, email, phone, unambiguous co-reference in the same episode — name similarity alone never suffices); suspicious same-name ⇒ new entity with `provisional=1`; `merge_ledger` rows for every merge **and** split; split op restores pre-merge state from the ledger.
- `kahyad/internal/factengine/evidentiality.go` — enum validation + deterministic -mış-morphology unit-test fixtures.
- CLI (user is the confirmer): `kahya entity merge <a> <b> --evidence <fact_id>`, `kahya entity split <merge_ledger_id>`, `kahya fact confirm <id>` (lifts agent_derived quarantine), `kahya fact retract <id>`. Turkish output strings.
- Injection eligibility predicate exported to W12-05: eligible ⇔ `status=active` AND log-odds ≥ −0.847 AND (tier ≠ `agent_derived` OR user-confirmed).
- Tests: `kahyad/internal/factengine/*_test.go` with the Turkish fixtures below, byte-exact.

## Steps

1. Read HANDOFF §5 memory #1–#4, the §5 ⚑ schema block, §4 routing. Inspect the W12-02 sqlc layer, W12-05 memory MCP, and W5-02 promotion call-site as built.
2. Implement candidate validation (struct schema, length+charclass caps, `extractor_ver` required, evidentiality enum with `inferred` default) and Go-side `source_tier` assignment per the provenance table in Context (extractor-proposed tiers ignored and clamped to `agent_derived`, ledgered; `user_asserted` requires a clean W4-03 taint record for the originating session — missing record ⇒ fail-closed).
3. Implement the lattice: tier ordering, per-tier confidence caps, quarantine for `agent_derived` (excluded from injection until `kahya fact confirm` or a W5-03 ✅ Doğru).
4. Implement log-odds updates with the constants above; per-(fact, session) evidence dedupe; denial/failed-freshness-probe decrement; <0.3 exit from injection.
5. Implement retraction detection (deterministic Turkish patterns + extractor-flagged retractions) and fact closure via `valid_to`/`status=retracted`.
6. Implement entity resolution/merge/split with `merge_ledger`; wire the CLI subcommands over UDS.
7. Route all existing writers (W12-05 `memory_write`, W5-02 promotion, W5-03 answers) through `WriteFact` — grep for any direct `INSERT INTO facts` outside the engine and remove it.
8. Swap the injection-eligibility predicate into W12-05 search/injection.
9. Write the tests; wire into `make test`.

### Turkish test fixtures (byte-exact — do not translate or ASCII-fold)

- Evidentiality: `"Emre işten ayrılmış."` ⇒ `reported`; `"Emre işten ayrıldı, bugün konuştuk."` ⇒ `witnessed`; `"Toplantı iyi geçmiş."` ⇒ `reported`.
- Retraction: seed `"Kahveyi çok seviyorum."` (fact: user likes coffee), then `"Kahveyi artık sevmiyorum."` ⇒ original fact `status=retracted`, `valid_to` set, negative evidence row.
- Namesakes: `"Emre (gold-token ekibinden) NATS konusunda yardımcı oldu."` and `"Spor salonundan Emre yarın gelemeyecekmiş."` ⇒ two distinct entities, no auto-merge, second is provisional; adding shared distinguishing evidence + `kahya entity merge` merges them; `kahya entity split` restores both.

## Acceptance criteria

- [ ] `make test` green including all fixtures above.
- [ ] Test: an `agent_derived` fact is absent from `memory_search` injection-eligible output; after `kahya fact confirm <id>` (or a W5-03 ✅ Doğru) it appears; its confidence never exceeds the 0.4 tier cap.
- [ ] Test: two same-session assertions of one fact produce exactly one evidence row; a third from a different session produces a second row and raises log-odds (no noisy-OR ratchet — assert exact expected values).
- [ ] Test: a user denial drops a p≈0.8 fact below 0.3 ⇒ excluded from injection; ledger event recorded.
- [ ] Test: the namesake fixture yields 2 entities and 0 `merge_ledger` merge rows without distinguishing evidence; merge then split round-trips via `merge_ledger` (row count 2: one merge, one split).
- [ ] Test: retraction fixture closes the fact (`status=retracted`, `valid_to NOT NULL`) and the retracted fact no longer injects.
- [ ] `grep -rn "INSERT INTO facts" kahyad/ --include='*.go' --include='*.sql'` shows writes only in the factengine/sqlc path (no bypass writers).
- [ ] Test: an extractor candidate struct claiming `source_tier=user_asserted` is stored as `agent_derived` (quarantined, excluded from injection) and the clamping is ledgered — the model cannot mint trust.
- [ ] Test: a candidate marked direct-user-utterance from a session with taint tier `untrusted` (or with no W4-03 taint record at all) is NOT stored as `user_asserted` (fail-closed).
- [ ] Test: a secret-lane candidate whose extraction would require the cloud model is rejected fail-closed with the Turkish notice, never proxied to Anthropic (assert via forward-proxy log).

## Out of scope

- Full two-temporal fact/predicate **graph queries** — v2 per HANDOFF §8 (columns exist and are populated; no graph engine).
- Profile-card builder/UI and reflex injection surfaces beyond the eligibility predicate.
- Retrieval eval harness and precision gate — W5-05 mini-baseline, W78-01 full eval.
- Reranker, embedding upgrades/`model_ver` migration (W12-11), SwiftUI memory browser (§8).
- Consolidation flow itself (W5-02) and ritual transport (W5-03) — this task only owns the write path they call.
