-- 0023_anchor_local_fallback: the one G07 migration (G00 C-1).
--
-- (1) FIX-P-15: anchor_log.local_fallback_at records when THIS row's
-- anchor line was appended to anchor.local_fallback_path (HANDOFF S5
-- safety #4: "offline'da farkli-uid'li yerel ekleme-yalniz dosya"). NULL =
-- not written yet; kahyad/internal/anchor.Pusher.Run writes it BEFORE any
-- remote push attempt, once per row (never per attempt), and retries it on
-- the next tick after a failure. Rows already 'pushed' under the pre-G07
-- code had their line appended to the fallback right after the push (when
-- a fallback path was configured), so they are backfilled as written to
-- avoid re-appending history. (0010's "retried, never duplicated" comment
-- held for the anchor_log row only; since FIX-P-13 it holds for the remote
-- anchors.log line too.)
--
-- (2) FIX-P-3 / G07-2 (handbook S-3): ledger_chain_base records the last
-- LEGACY event id - an event written before 0010 seeded
-- ledger_digest_state, which the running digest never folded in. `kahya
-- ledger verify` recomputes the digest from genesis over events with id >
-- base_event_id; legacy events stay outside the external anchor's coverage
-- (they were never anchored) and verify reports how many there are. The
-- row is written once, here, and never by Go code:
--   * last_event_id = 0 (no event chained since 0010 - including a
--     pre-0010 database migrating 0010 and 0023 in the same run): every
--     existing event is legacy, base = MAX(events.id) (0 when empty). Exact.
--   * last_event_id > 0 (0010 applied earlier, events chained since): the
--     legacy events are the ones created before 0010 was applied (its
--     goose_db_version tstamp, second precision). A wrong guess here can
--     only make verify fail loudly (digest/state mismatch, or a checkpoint
--     at or below the base), never hide an anchored event.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
ALTER TABLE anchor_log ADD COLUMN local_fallback_at TEXT;
UPDATE anchor_log SET local_fallback_at = anchored_at WHERE status = 'pushed';

CREATE TABLE ledger_chain_base (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    base_event_id INTEGER NOT NULL CHECK (base_event_id >= 0)
);

INSERT INTO ledger_chain_base (id, base_event_id)
SELECT 1, CASE
    WHEN (SELECT last_event_id FROM ledger_digest_state WHERE id = 1) = 0
        THEN COALESCE((SELECT MAX(id) FROM events), 0)
    ELSE COALESCE((
        SELECT MAX(e.id) FROM events e
        WHERE datetime(e.created_at) < (
            SELECT g.tstamp FROM goose_db_version g
            WHERE g.version_id = 10 AND g.is_applied = 1
            ORDER BY g.id DESC LIMIT 1)
    ), 0)
END;

-- +goose Down
DROP TABLE IF EXISTS ledger_chain_base;
ALTER TABLE anchor_log DROP COLUMN local_fallback_at;
