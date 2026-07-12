-- 0009_session_taint: the W4-03 Reader/Actor taint-tier persistence
-- (HANDOFF S5 safety #2 flag: "taint katmani session_id anahtariyla
-- SQLite'ta kalici saklanir, resume'da yeniden yuklenir ve yalniz
-- yukselir - asla dusmez; kayit yoksa oturum guvenilmez sayilir
-- (fail-closed)").
--
-- session_id is the PRIMARY KEY (not a FK to tasks.id/tasks.session_id):
-- a session_taint row's lifetime is keyed on the AGENT SDK session_id, the
-- same identity kahyad/internal/task's own tasks.session_id column
-- already carries (a task may be resumed under the SAME session_id across
-- multiple tasks rows, and a Reader/Actor-seeded session may never even
-- have a tasks row of its own at the moment its taint row is created -
-- see kahyad/internal/taint's own doc comment for the two exact call
-- sites that ever insert a 'clean' row).
--
-- tier is a two-value CHECK enum, never a boolean: 'clean' | 'tainted'.
-- reason is free-form (NULL for a plain clean-row insert; the raising
-- cause - e.g. "untrusted_output:web_fetch" - for every Raise call). There
-- is deliberately NO trigger/constraint here that forbids an UPDATE from
-- 'tainted' back to 'clean' at the SQL level - that invariant is enforced
-- entirely in Go (kahyad/internal/taint.Tracker: there is simply no SQL
-- statement in this codebase that ever sets tier='clean' on an EXISTING
-- row - InsertSessionTaintClean below is a plain INSERT, not an upsert, so
-- it can only ever succeed against a session_id with no existing row at
-- all).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE TABLE session_taint (
    session_id TEXT PRIMARY KEY,
    tier       TEXT NOT NULL CHECK (tier IN ('clean', 'tainted')),
    reason     TEXT,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS session_taint;
