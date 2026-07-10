# W12-03 — FTS5 dual index + sqlite-vec table + search API

**Status:** done
**Phase:** W1–2 — Core
**Depends on:** W12-02
**Flags:** none
**Handoff refs:** §4 stack ⚑, §6 W1–2

## Goal
kahyad can rank chunks for a Turkish query. Two FTS5 indexes (trigram + unicode61) with BM25 score fusion, the `sqlite-vec` table (with `model_ver`, vectors filled later by W12-11), and a Go search API + debug endpoint. This is the component that must make `'evlerimizden'` find a note containing `'ev'`.

## Context you need
The binding stack decision (HANDOFF §4, verbatim):

> | Veritabanı | Tek **SQLite** (WAL) + `sqlite-vec` (≥0.1.9 pinle; brute-force KNN, ≤~100k vektörde yeterli) + FTS5. ⚑ **FTS5 çift indeks:** `tokenize='trigram'` (Türkçe ek/İ-ı duyarsız kısmi eşleşme) + `unicode61` (kesin terim/kod); BM25 skorları füzyonda birleşir. Şifreleme: FileVault + Keychain (SQLCipher **değil**) |

The vec table must carry `model_ver` even though W1–2 acceptance is FTS5-only (HANDOFF §6 ⚑): "W1–2 kabulü FTS5-only aramayla sağlanır — … (şemadaki embedding kolonları + `model_ver` yine gün 1'de açılır …)". And the usage rule it exists for (HANDOFF §4 ⚑):

> ⚑ **`model_ver` kullanım kuralı:** her vektör satırı `model_ver` taşır; KNN sorguları daima **tek aktif `model_ver`'e filtrelenir** (karışık-versiyon KNN yasak).

Target behavior fixed by the W1–2 gate (HANDOFF §6): "**`'evlerimizden'` sorgusu `'ev'` içeren tohum notu buluyor** (Türkçe morfoloji)". tasks/README.md forbids manual stemming — no Turkish suffix tables, no morphological analyzer; matching must come from the trigram index plus language-agnostic mechanics (character truncation is allowed; suffix-stripping rules are not).

Gotchas: the FTS5 `trigram` tokenizer only matches query substrings of ≥3 chars, and SQLite's built-in case folding does NOT reliably fold Turkish `İ→i` / `I→ı`. Therefore the trigram leg indexes a **folded copy** of the text (fold at index time and query time with the same function); stored `chunks.text` stays byte-exact. kahyad is the sole writer, so FTS rows are maintained in the same transaction as `chunks` rows (no triggers needed).

Prior output: W12-02 store layer + migration framework. sqlite-vec Go side: `github.com/asg017/sqlite-vec-go-bindings/cgo` pinned to the release wrapping sqlite-vec ≥0.1.9 (register once; vec0 available on all conns).

## Deliverables
- `kahyad/migrations/0002_fts_vec.sql` — both FTS5 tables + vec0 table (vec0 DDL runs via a registered driver, so migrations must execute on a connection with sqlite-vec loaded).
- `kahyad/internal/textnorm/fold.go` + `fold_test.go` — deterministic fold: NFC → map `İ→i, I→ı, ı→ı` then Unicode lowercase → final map `ı→i` (net effect: `İ/I/i/ı` all become `i`), diacritics kept otherwise.
- `kahyad/internal/search/search.go` + `search_test.go` — fusion search API.
- `kahyad/internal/search/ftswrite.go` — helpers the indexer (W12-04) calls to upsert/delete FTS rows transactionally with chunks.
- UDS endpoint `POST /v1/memory/search` (raw ranking; the `<hafiza>` rendering layer on top is W12-05).

## Steps
1. Migration `0002_fts_vec.sql`:
   - `CREATE VIRTUAL TABLE chunks_fts_tri USING fts5(text_folded, tokenize='trigram');` — rowid = `chunks.id`.
   - `CREATE VIRTUAL TABLE chunks_fts_uni USING fts5(text, tokenize='unicode61');` — rowid = `chunks.id`.
   - `CREATE VIRTUAL TABLE chunk_vec USING vec0(chunk_id INTEGER PRIMARY KEY, embedding FLOAT[512] distance_metric=cosine, model_ver TEXT);` (512-dim per §4: `Qwen3-Embedding-0.6B` 512-dim MRL).
   - At startup assert `sqlite_version() >= 3.45` and `vec_version()` succeeds; fail-fast otherwise with `"event":"sqlite_feature_missing"`.
2. `ftswrite.go`: `IndexChunk(tx, chunkID, text)` inserts into both FTS tables (`text_folded = textnorm.Fold(text)`); `DeleteChunk(tx, chunkID)` removes from both + `chunk_vec`.
3. `search.go` — `Search(ctx, q string, k int) ([]Hit, error)`, `Hit{ChunkID, EpisodeID, Path, Text, Score, SourceTier}`:
   a. **unicode61 leg:** `SELECT rowid, bm25(chunks_fts_uni) FROM chunks_fts_uni WHERE chunks_fts_uni MATCH ?` with the query tokens quoted as phrases; take top 50.
   b. **trigram leg** (on folded query `fq = textnorm.Fold(q)`): for each whitespace token `t` of `fq`:
      - if `len(t) ≥ 3` runes: try `MATCH '"t"'`; while it yields 0 rows and `len(t) > 3`, truncate one trailing rune and retry (language-agnostic relaxation, not stemming).
      - if the MATCH ladder bottoms out at 3 runes with 0 rows (or the token started with <3 runes, where trigram MATCH is impossible), continue the SAME one-trailing-rune truncation below the trigram floor as a **Go substring scan**: for stem lengths from `min(len(t),3)−1` down to `2`, load all chunks and keep those where `strings.Contains(textnorm.Fold(chunk.text), stem)`; stop at the first length yielding ≥1 hit (for `evlerimizden` the MATCH ladder dies at `evl`, then the scan succeeds at stem `ev`). Corpus ≤~100k chunks per §4 — brute force is in-budget. Scan hits get a fixed post-normalization tri-leg score of `0.1` (config `scan_floor_score`) so genuine trigram MATCH hits always outrank scan hits.
      Union rows per token; per-row leg score = best token score (normalized BM25 for MATCH rows, floor for scan rows).
   c. **fusion:** negate BM25 (SQLite returns lower=better), min-max normalize within each leg, `fused = 0.6*tri + 0.4*uni` (weights in config, defaults committed), missing leg contributes 0; dedupe by chunk_id keeping the max; sort desc, tie-break by `chunks.id` desc; return top k joined with `episodes` for path/tier.
   d. Log one JSONL line per search: `event=memory_search`, `trace_id`, query length, k, leg hit counts, duration_ms (never log the query text at info level — it may be sensitive; debug level only).
4. Endpoint `POST /v1/memory/search` `{query, k?, trace_id?}` → `{results:[{chunk_id,episode_id,path,text,score,source_tier}]}`. This is the raw internal API; W12-05 adds `for_injection` semantics on top.
5. Tests (fixtures byte-exact Turkish; insert via store helpers):
   - Chunk A: `İstanbul'da yeni bir ev bakıyoruz; Kadıköy'de iki daire gezdik.` Chunk B: `gold-token servisinde NATS saga deseni ve trace_id correlation notları.`
   - Query `evlerimizden` → A is rank 1 (relaxation ladder: `evlerimizden`→…→`evl`→ scan-stem `ev`).
   - Query `istanbul` → A hits (İ→i fold works both directions); query `İSTANBUL` → same.
   - Query `trace_id` → B rank 1 via unicode61 exact term.
   - Query `saga deseni` → B rank 1; fused score of B > any score of A.
   - Empty query → error, no panic. `k=0` → default 8.
   - Fold function: `Fold("Iıİi") == "iiii"` documented + asserted; NFC applied (fixture with decomposed `e‌+◌̂`).
   - vec table: `INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (…)` accepts a 512-float blob and rejects 511 (dimension enforced) — proves the table is real even though search ignores it until W12-11.

## Acceptance criteria
- [ ] `go test ./kahyad/internal/search/... ./kahyad/internal/textnorm/...` green in `make test`, including every fixture case in step 5 (the `'evlerimizden'`→`'ev'` case is the §6 gate in miniature — it must be a permanent test, not a manual check).
- [ ] `sqlite3 brain.db ".tables"` additionally lists `chunks_fts_tri chunks_fts_uni chunk_vec` after boot on a fresh dir.
- [ ] With two chunks seeded (test helper or W12-04): `curl -s --unix-socket ... -XPOST http://kahyad/v1/memory/search -d '{"query":"evlerimizden","k":3}' | jq -e '.results[0].text | contains("ev ")'` exits 0.
- [ ] Search JSONL lines carry `trace_id` and `duration_ms`; no info-level line contains the query text.
- [ ] `PRAGMA user_version` now = 2; boot on a v1 DB migrates cleanly (test: open a DB created by 0001 only, boot, assert tables).

## Out of scope
- Filling `chunk_vec` with real vectors, KNN leg in fusion, MLX embedding service — W12-11 (slidable to W3–4; §6 timing note).
- Indexing real files from `~/Kahya/memory` — W12-04.
- `for_injection` filtering, `<hafiza>` block rendering, MCP tools — W12-05.
- Reranker (§8 deferred: "yalnız eval precision düşerse"); Turkish morphological analysis of any kind.
