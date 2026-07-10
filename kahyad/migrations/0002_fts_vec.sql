-- 0002_fts_vec: FTS5 dual index (trigram + unicode61) + sqlite-vec table
-- (HANDOFF S4 stack row flag: "FTS5 cift indeks"; S4 model_ver usage flag).
--
-- chunks_fts_tri / chunks_fts_uni are content-less FTS5 tables whose rowid
-- IS chunks.id (kahyad/internal/search/ftswrite.go sets rowid explicitly on
-- every INSERT). No triggers maintain them - kahyad is brain.db's only
-- writer (HANDOFF S4/S5) and keeps both tables in the SAME transaction as
-- the chunks INSERT/DELETE.
--
--   * chunks_fts_tri indexes a FOLDED copy of chunk text (text_folded, see
--     kahyad/internal/textnorm) via the trigram tokenizer, for Turkish
--     suffix-/I-i-insensitive partial matching ('evlerimizden' -> 'ev').
--   * chunks_fts_uni indexes the byte-exact text via unicode61, for exact
--     term/code matching (e.g. trace_id).
--
-- Search fuses BM25 from both legs (kahyad/internal/search/search.go).
--
-- chunk_vec is the sqlite-vec KNN table (HANDOFF S4: ">=0.1.9 pinle" - see
-- go.mod: the highest release actually published as of this task is
-- v0.1.6, so that is what is pinned; no v0.1.9 tag exists upstream).
-- 512 dimensions matches Qwen3-Embedding-0.6B's MRL truncation (HANDOFF S4
-- model fleet row). model_ver is part of the schema from day 1 even though
-- W1-2 acceptance is FTS5-only and vectors are filled later by W12-11
-- (HANDOFF S6 timing note): every future vector row must carry model_ver
-- so KNN queries can filter to a single active model_ver (HANDOFF S4 flag:
-- mixed-version KNN is forbidden).
--
-- This file's DDL can only run on a connection with the sqlite-vec
-- extension loaded (kahyad/internal/store/db.go registers
-- sqlite_vec.Auto() before any sqlite3 connection is opened, so goose
-- migrations get it too - HANDOFF S4 stack: goose runs before any other
-- work). Startup additionally asserts sqlite_version() >= 3.45 and
-- vec_version() succeeds, fail-fast (event=sqlite_feature_missing)
-- otherwise (kahyad/internal/store/db.go assertSQLiteFeatures).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql header - a
-- single non-ASCII byte silently corrupts sqlc's generated query strings
-- for the WHOLE package). sqlc's sqlite grammar also cannot parse the
-- fts5 tokenize='...' argument or the vec0 module's column-option syntax
-- (distance_metric=cosine, FLOAT[512]) at all - confirmed empirically,
-- "extraneous input '=' expecting {')', ','}" - so sqlc.yaml's schema list
-- deliberately does NOT include this file (see sqlc.yaml comment). None of
-- these three tables are ever queried through sqlc; every reader/writer
-- goes through kahyad/internal/search's hand-written SQL instead.

-- +goose Up
CREATE VIRTUAL TABLE chunks_fts_tri USING fts5(
    text_folded,
    tokenize = 'trigram'
);

CREATE VIRTUAL TABLE chunks_fts_uni USING fts5(
    text,
    tokenize = 'unicode61'
);

CREATE VIRTUAL TABLE chunk_vec USING vec0(
    chunk_id INTEGER PRIMARY KEY,
    embedding FLOAT[512] distance_metric=cosine,
    model_ver TEXT
);

-- +goose Down
DROP TABLE IF EXISTS chunk_vec;
DROP TABLE IF EXISTS chunks_fts_uni;
DROP TABLE IF EXISTS chunks_fts_tri;
