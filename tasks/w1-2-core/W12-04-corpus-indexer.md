# W12-04 — memory corpus indexer

**Status:** todo
**Phase:** W1–2 — Core
**Depends on:** W12-03, W0-01
**Flags:** none
**Handoff refs:** §4 memory, §6 W1–2

## Goal
The seeded markdown corpus in `~/Kahya/memory` is searchable. kahyad walks the memory repo, creates one episode per file and chunk rows + FTS entries per file, re-indexes incrementally by file hash, and exposes a reindex trigger endpoint. After this task, W12-03's search returns real seed notes.

## Context you need
Source-of-truth split (HANDOFF §4, verbatim):

> | Hafıza kaynak gerçeği | **Markdown + git** (`~/Kahya/memory/*.md`); SQLite türetilmiş indeks |

SQLite is *derived*: the whole index must be reproducible from markdown at any time (that's also the W7-8 restore-drill assumption). Single-writer rule (HANDOFF §6 W1–2, verbatim): "hafıza MCP sunucusu (`memory_search` / `memory_write` / `memory_forget`, **kahyad içinde Go — brain.db'nin TEK yazarı kahyad'dır**)" — the indexer runs inside kahyad, nothing else ever writes brain.db.

Seed tier (HANDOFF §7 ⚑, binds the default `source_tier` you stamp on seeded files):

> ⚑ **Tohum tier eşlemesi:** tohum dosyaları içe aktarımda kullanıcı **10 dakikalık tek seferlik gözden geçirmeden** geçirir (yanlış/bayat notları siler); sağ çıkan olgular `source_tier=user_asserted` (≤.95) alır — gerekçe: kullanıcının kendi oturumlarında biriktirip fiilen sahiplendiği notlar, gözden geçirme karantinayı kaldıran kullanıcı onayı sayılır. Böylece §5-Hafıza-#1 karantina kuralı bozulmadan W1–2 kabul kriteri (tohumdan `<hafiza>` enjeksiyonu) çalışır.

W0-01 already created and seeded `~/Kahya/memory` (its own git repo) and the user did the review — so everything currently in the corpus indexes as `source_tier='user_asserted'`. Files later written by the agent (W12-05 `memory_write`) will carry their own tier; the indexer must honor a per-file tier override (see step 2), defaulting to `user_asserted`.

Chunking must be language-agnostic (backlog row): operate on runes/paragraphs, never on a language-specific tokenizer. Keep Turkish text byte-exact — normalization happens only in W12-03's folded FTS copy.

## Deliverables
- `kahyad/internal/indexer/indexer.go` + `indexer_test.go` — walk, hash, chunk, upsert.
- `kahyad/internal/indexer/chunker.go` + `chunker_test.go` — heading/paragraph chunker.
- UDS endpoint `POST /v1/reindex` (`{"full": false}` default) → `{"files_indexed":n,"files_unchanged":n,"files_removed":n,"chunks":n,"duration_ms":n}`.
- Startup hook: incremental reindex on every kahyad boot, after migrations, before serving is fine to run async (log `event=reindex_done`).
- sqlc queries added as needed (`GetEpisodeByPath`, `UpsertEpisode`, `MarkEpisodeDeleted`, …).

## Steps
1. Walk `cfg.memory_dir` recursively for `*.md`; skip `.git/**`, `.trash/**` (used by `memory_forget`, W12-05), and dotfiles. Compute SHA-256 of file bytes.
2. Sidecar tier support: if the file's YAML front-matter contains `kahya_source_tier: <tier>` (one of the five §5 enum values), use it; else default `user_asserted`. (W12-05's `memory_write` stamps `agent_derived` this way — decide the mechanism here so both tasks agree.)
3. Per file, compare hash with `episodes.source_hash` for `source='memory_file' AND source_path=<repo-relative path>`:
   - unchanged → skip;
   - new/changed → in ONE transaction: upsert episode (`source='memory_file'`, `source_path`, `source_hash`, `source_tier`, `status='active'`), delete old chunks via `ftswrite.DeleteChunk` + `DeleteChunksByEpisode`, insert new chunks with `seq` 0..n and `IndexChunk` each;
   - file gone from disk → `status='deleted'` on episode + delete its chunks/FTS rows (derived index mirrors the source of truth; git history in `~/Kahya` keeps the past).
4. Chunker: first strip the YAML front-matter block (leading `---\n…\n---\n`, if present) — its tier value is read in step 2 but its bytes must never be indexed or searchable; then split on markdown headings (`#`–`###`); inside a section, split on blank-line paragraph boundaries; merge adjacent pieces up to **1600 runes max** per chunk with **200-rune overlap** between consecutive chunks; never split inside a fenced code block. Chunk `content_hash` = SHA-256 of chunk text.
5. `POST /v1/reindex`: `full:true` forces re-chunk of every file (ignores hash match — needed after chunker changes); mint/propagate `trace_id`; write a ledger event `kind='reindex'` with the summary payload. Serialize reindex runs with a mutex (second concurrent call returns `409 {"error":"reindex zaten çalışıyor"}`).
6. The user-facing `kahya reindex` command that calls this endpoint ships with the CLI in W12-06 — here, verify via `curl` only.
7. Tests (fixture corpus in `t.TempDir()`, byte-exact Turkish content):
   - `ev-notlari.md`: `# Ev arayışı\n\nİstanbul'da yeni bir ev bakıyoruz; Kadıköy öne çıktı.\n` → 1 episode, ≥1 chunk; search `evlerimizden` finds it (integration with W12-03).
   - idempotency: second run indexes 0 files;
   - edit file → exactly that episode re-chunked, chunk ids replaced, FTS consistent (`INSERT INTO chunks_fts_tri(chunks_fts_tri) VALUES('integrity-check')` passes);
   - delete file → episode `status='deleted'`, its text no longer searchable;
   - front-matter `kahya_source_tier: agent_derived` → episode row carries it, AND the literal string `kahya_source_tier` is NOT findable via `/v1/memory/search` (front-matter stripped before chunking);
   - a >5000-rune file produces overlapping chunks each ≤1600 runes;
   - `.trash/foo.md` and `.git` contents are never indexed.

## Acceptance criteria
- [ ] `make test` green including all step-7 tests.
- [ ] Against the real seeded corpus: `curl -s --unix-socket "$HOME/Library/Application Support/Kahya/kahyad.sock" -XPOST http://kahyad/v1/reindex -d '{}' | jq .` reports `files_indexed` ≥ the number of seed files, and `sqlite3 brain.db "SELECT count(*) FROM episodes WHERE source='memory_file' AND status='active';"` matches `find ~/Kahya/memory -name '*.md' -not -path '*/.git/*' -not -path '*/.trash/*' | wc -l`.
- [ ] Immediately re-running the same curl reports `files_indexed: 0` (hash-incremental).
- [ ] `sqlite3 brain.db "SELECT DISTINCT source_tier FROM episodes WHERE source='memory_file';"` = `user_asserted` (seed tier mapping honored).
- [ ] `POST /v1/memory/search {"query":"evlerimizden"}` over UDS returns a seed-corpus note containing `ev` as a top-3 result (pre-gate for W12-10).
- [ ] Ledger: `sqlite3 brain.db "SELECT count(*) FROM events WHERE kind='reindex';"` ≥ 1 and the payload JSON includes the summary counts; the reindex JSONL log lines share one `trace_id`.

## Out of scope
- Writing/deleting markdown (`memory_write`/`memory_forget`) — W12-05. Git operations in `~/Kahya` — W12-05 (writes) and W5-02 (consolidation commits).
- Fact extraction into `facts`/`entities`/`evidence` — W5-04. This task populates episodes/chunks only.
- Embeddings for chunks — W12-11.
- Watching the filesystem (FSEvents) — not in HANDOFF; reindex is boot + explicit trigger only for MVP.
