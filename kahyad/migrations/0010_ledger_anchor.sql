-- 0010_ledger_anchor: W4-05 dis-cana defter (external-anchor, tamper-evident
-- ledger). HANDOFF S5 safety #4 flag, quoted verbatim in the task spec:
-- "kahyad her N saatte defterin son event hash'ini yalniz-append yetkili
-- ayri uzak hedefe yazar". This migration adds the two tables that make
-- that mechanism possible; the actual digest math lives in Go
-- (kahyad/internal/ledgerdigest), never in SQL.
--
-- ledger_digest_state is a SINGLE-ROW table (CHECK(id=1) - never more than
-- one running-digest checkpoint exists): last_event_id is the highest
-- events.id folded into digest so far, and digest is the running
-- SHA-256(prev_digest || uint64_be(event_id) || event_payload_bytes) chain
-- (genesis prev_digest = 32 zero bytes). kahyad/internal/store.
-- InsertEventWithDigest is the ONLY writer of this table - it advances
-- last_event_id/digest in the SAME SQLite transaction as the events INSERT
-- it guards, seeded here with the genesis row so the very first ledger
-- append in a fresh brain.db has a prev_digest to chain from.
--
-- anchor_log is one row per anchor ATTEMPT (not one row per tick - a tick
-- that finds nothing new to anchor writes no row at all): event_id/
-- digest_hex is the ledger_digest_state snapshot this attempt is anchoring,
-- status starts 'pending' (row inserted before the git push is even
-- attempted) and flips to 'pushed' only once the anchor line has actually
-- landed on the remote - kahyad/internal/anchor/push.go's Pusher is the
-- only writer. A row stuck at 'pending' across ticks (remote unreachable)
-- is retried, never duplicated - see push.go's own doc comment.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE TABLE ledger_digest_state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    last_event_id INTEGER NOT NULL,
    digest        BLOB NOT NULL
);

-- Genesis row: last_event_id=0 (no event folded in yet), digest=32 zero
-- bytes (the fixed genesis prev_digest every fresh brain.db chains its
-- first ledger append from).
INSERT INTO ledger_digest_state (id, last_event_id, digest)
VALUES (1, 0, zeroblob(32));

CREATE TABLE anchor_log (
    id          INTEGER PRIMARY KEY,
    event_id    INTEGER NOT NULL,
    digest_hex  TEXT NOT NULL,
    anchored_at TEXT NOT NULL,
    remote_ref  TEXT,
    status      TEXT NOT NULL CHECK (status IN ('pending', 'pushed'))
);

CREATE INDEX idx_anchor_log_status ON anchor_log(status);
CREATE INDEX idx_anchor_log_event_id ON anchor_log(event_id);

-- +goose Down
DROP INDEX IF EXISTS idx_anchor_log_event_id;
DROP INDEX IF EXISTS idx_anchor_log_status;
DROP TABLE IF EXISTS anchor_log;
DROP TABLE IF EXISTS ledger_digest_state;
