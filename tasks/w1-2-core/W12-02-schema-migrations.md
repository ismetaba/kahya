# W12-02 — schema + goose migrations

**Status:** done
**Phase:** W1–2 — Core
**Depends on:** W12-01
**Flags:** none
**Handoff refs:** §5 schema block, §4 stack

## Goal
`brain.db` exists with the full Day-1 schema. kahyad runs goose migrations at startup before doing any other work, applies the WAL/busy_timeout/checkpoint pragmas, and exposes a typed, sqlc-generated query layer. All §5-mandated tables exist (empty is fine) from this task onward.

## Context you need
Stack rows (HANDOFF §4, verbatim):

> | Kontrol düzlemi | **Go** + `sqlc` üretimli sorgular. ⚑ **Migrasyon: `goose`/`golang-migrate`** (sqlc migrasyon yapmaz), kahyad açılışında her işten önce koşar; sürüm `PRAGMA user_version` |

> | Veritabanı | Tek **SQLite** (WAL) + `sqlite-vec` (≥0.1.9 pinle; brute-force KNN, ≤~100k vektörde yeterli) + FTS5. ⚑ **FTS5 çift indeks:** `tokenize='trigram'` (Türkçe ek/İ-ı duyarsız kısmi eşleşme) + `unicode61` (kesin terim/kod); BM25 skorları füzyonda birleşir. Şifreleme: FileVault + Keychain (SQLCipher **değil**) |

The mandatory table list (HANDOFF §5, verbatim):

> ⚑ **Şema — W1–2'de açılması ZORUNLU tablolar** (yukarıdaki #2–#4 sonradan türetilemez; kanıt↔oturum bağı yakalama-anı verisidir):
> ```
> episodes, chunks,
> facts (subject, predicate, object, source_tier, evidentiality [-mış morfolojisi:
>        witnessed|reported|inferred], confidence [log-odds], importance,
>        valid_from/to, status, evidence, extractor_ver),
> entities + entity_aliases,
> evidence (fact_id, episode_id, session_id, polarity ±),
> merge_ledger (birleştirme/bölme kayıtları),
> tasks, events/outbox
> ```
> İki-zamanlı sorgular (valid_from/to) MVP'de kolon olarak var ama tam graf sorgusu v2 (§8). Tablolar boş başlayabilir; kritik olan **gün 1'de var olmaları**.

`tasks` must be able to carry taint from day 1 (HANDOFF §5 safety #2 ⚑): "taint katmanı `session_id` anahtarıyla SQLite'ta (tasks/sessions) **kalıcı saklanır**, resume'da yeniden yüklenir ve **yalnız yükselir — asla düşmez**; kayıt yoksa oturum güvenilmez sayılır (fail-closed)." — you only add the column here; W4-03 implements the behavior.

`events` is the ledger: "defter (append-only events)" (§4). Enforce append-only at the SQL level with triggers.

Prior output: W12-01 gives config (`db_path`, env overrides) and JSONL logging. Driver note: use `mattn/go-sqlite3` (cgo — needed by sqlite-vec Go bindings in W12-03 and gives a recent bundled SQLite with FTS5 trigram); **build every Go target with `-tags sqlite_fts5`** — FTS5 is NOT compiled into mattn's default build; put the tag into the Makefile `build`/`test`/`lint` targets once here, or W12-03's `CREATE VIRTUAL TABLE … USING fts5` migration will fail at boot. goose = `github.com/pressly/goose/v3` with migrations embedded via `embed.FS`. Pin both in go.mod.

## Deliverables
- `kahyad/migrations/0001_init_schema.sql` — goose up/down for all tables below.
- `kahyad/internal/store/db.go` — open DB, apply pragmas, run migrations, set `PRAGMA user_version`.
- `kahyad/internal/store/queries/*.sql` + `sqlc.yaml` (repo root or `kahyad/`) + generated package `kahyad/internal/store` (checked in).
- `kahyad/internal/store/store_test.go` — migration + pragma + append-only tests.
- kahyad startup wiring in `kahyad/main.go`: open+migrate **before** the UDS listener starts serving.
- `/health` extended with `"db":"ok"` and `"schema_version":<user_version>`.

## Steps
1. Write `0001_init_schema.sql` (goose `-- +goose Up` / `-- +goose Down`). Tables and required columns (add `id INTEGER PRIMARY KEY` and `created_at TEXT NOT NULL` everywhere; timestamps RFC3339 UTC):
   - `episodes(source TEXT NOT NULL, source_path TEXT, source_hash TEXT, source_tier TEXT NOT NULL DEFAULT 'user_asserted' CHECK(source_tier IN ('user_edit','user_asserted','external_doc','screen','agent_derived')), started_at TEXT, ended_at TEXT, status TEXT NOT NULL DEFAULT 'active', meta TEXT)` — `source_tier` here drives injection eligibility for corpus chunks (W12-05).
   - `chunks(episode_id INTEGER NOT NULL REFERENCES episodes(id), seq INTEGER NOT NULL, text TEXT NOT NULL, content_hash TEXT NOT NULL, UNIQUE(episode_id, seq))`.
   - `facts(subject TEXT NOT NULL, predicate TEXT NOT NULL, object TEXT NOT NULL, source_tier TEXT NOT NULL CHECK(...same enum...), evidentiality TEXT NOT NULL CHECK(evidentiality IN ('witnessed','reported','inferred')), confidence REAL NOT NULL, importance REAL NOT NULL DEFAULT 0, valid_from TEXT, valid_to TEXT, status TEXT NOT NULL DEFAULT 'active', evidence TEXT, extractor_ver TEXT, updated_at TEXT NOT NULL)` — `confidence` is **log-odds** (REAL, can be negative); comment this in the DDL.
   - `entities(canonical_name TEXT NOT NULL, kind TEXT, status TEXT NOT NULL DEFAULT 'active')` and `entity_aliases(entity_id INTEGER NOT NULL REFERENCES entities(id), alias TEXT NOT NULL)`.
   - `evidence(fact_id INTEGER NOT NULL REFERENCES facts(id), episode_id INTEGER REFERENCES episodes(id), session_id TEXT, polarity INTEGER NOT NULL CHECK(polarity IN (-1,1)))`.
   - `merge_ledger(op TEXT NOT NULL CHECK(op IN ('merge','split')), src_entity_id INTEGER, dst_entity_id INTEGER, evidence TEXT, actor TEXT NOT NULL)`.
   - `tasks(id TEXT PRIMARY KEY, trace_id TEXT NOT NULL, session_id TEXT, state TEXT NOT NULL, taint_tier TEXT NOT NULL DEFAULT 'untrusted', model TEXT, envelope TEXT, updated_at TEXT NOT NULL)` (task ids are minted strings, not rowids).
   - `events(id INTEGER PRIMARY KEY AUTOINCREMENT, trace_id TEXT NOT NULL, ts TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL)` + triggers `events_no_update`/`events_no_delete`: `BEFORE UPDATE/DELETE ON events ... SELECT RAISE(ABORT, 'ledger is append-only')`.
   - `outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, trace_id TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL, dispatched_at TEXT)` (§4: "MVP: SQLite outbox tablosu + goroutine'ler"; the dispatcher itself is W4-02).
   - Indexes: `events(trace_id)`, `events(kind, ts)`, `chunks(episode_id)`, `facts(subject)`, `evidence(fact_id)`, `tasks(trace_id)`, `tasks(session_id)`.
2. `store.Open(cfg)`: open with `_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL`; run `goose.Up` from the embedded FS; then set `PRAGMA user_version = <latest migration version>` (goose tracks its own table; `user_version` is the contract's cheap external version check — keep both in sync after every migration run); start a checkpoint policy: `PRAGMA wal_autocheckpoint=1000` plus a `wal_checkpoint(TRUNCATE)` on graceful shutdown.
3. Wire into `kahyad/main.go` before `server` starts accepting; on any migration error log `"event":"migrate_failed"` and exit 1 (fail-closed — no serving on a half-migrated DB).
4. sqlc: configure `sqlc.yaml` for the sqlite engine over `migrations/` schema; write starter queries used by later tasks (`InsertEvent`, `ListEventsByTrace`, `InsertTask`, `UpdateTaskState`, `GetTaskBySession`, `InsertEpisode`, `InsertChunk`, `DeleteChunksByEpisode`, `GetEpisodeByPath`); `make generate` target runs `sqlc generate`; commit generated code.
5. Tests (against `t.TempDir()` DB): fresh open creates all 10 tables (assert via `sqlite_master`); reopen is idempotent; `PRAGMA journal_mode` returns `wal`; `PRAGMA user_version` equals latest migration; UPDATE and DELETE on `events` fail with `ledger is append-only`; goose down/up round-trips; inserting a fact with `evidentiality='sanılan'` fails the CHECK (enum enforced).

## Acceptance criteria
- [ ] `make test` green; includes all tests from step 5.
- [ ] First `kahyad` boot on a clean `KAHYA_DATA_DIR`: JSONL log shows `"event":"migrations_applied"` before the first `"event":"http_request"`; `sqlite3 "$KAHYA_DATA_DIR/brain.db" ".tables"` lists `episodes chunks facts entities entity_aliases evidence merge_ledger tasks events outbox`.
- [ ] `sqlite3 brain.db "PRAGMA user_version;"` prints the latest migration number; `sqlite3 brain.db "PRAGMA journal_mode;"` prints `wal`.
- [ ] `sqlite3 brain.db "UPDATE events SET payload='x';"` fails with `ledger is append-only` (run after inserting one row via a test helper or `INSERT`).
- [ ] `curl --unix-socket ... /health` now returns `"db":"ok"` and the correct `schema_version`.
- [ ] `sqlc generate` produces no diff against the committed generated code (checked by a `make lint` step or CI-style `git diff --exit-code`).

## Out of scope
- FTS5 virtual tables, sqlite-vec table, any index-time text folding — W12-03 (migration `0002`).
- Populating episodes/chunks (indexer) — W12-04. Fact extraction/confidence math — W5-04.
- Outbox dispatcher, task state machine semantics, taint enforcement — W4-02/W4-03.
- Backups (`VACUUM INTO`), external ledger anchor — W4-06/W4-05.
- SQLCipher (§8 explicitly deferred; encryption = FileVault + Keychain).
