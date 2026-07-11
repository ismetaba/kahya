-- 0004_egress_budget: the W3-05 egress proxy's per-host daily byte-budget
-- persistence (HANDOFF S5 safety #1: "hedef allowlist + hacim butcesine
-- tabi"). kahyad/internal/egress/budget.go is the only writer of this
-- table. "day" is the Mac's LOCAL wall-clock day, time.Now().Format(
-- "2006-01-02") - see budget.go's own doc comment for why local (not UTC)
-- wall-clock day is deliberate (a human operator reasons about "today's
-- budget" in their own local day boundary, not UTC midnight). Counters
-- are keyed (host, day) so they survive a kahyad restart (persisted row,
-- not an in-memory-only counter) and roll over cleanly at the local day
-- boundary with no explicit reset job needed - a new day is simply a new
-- row, absent until the first byte crosses it.
--
-- This is a plain table (no FTS5/vec0 syntax), so unlike 0002_fts_vec.sql
-- it IS added to sqlc.yaml's schema list and has real sqlc-generated
-- queries (kahyad/internal/store/queries/queries.sql).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE TABLE egress_budget (
    host  TEXT NOT NULL,
    day   TEXT NOT NULL,
    bytes INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (host, day)
);

-- +goose Down
DROP TABLE IF EXISTS egress_budget;
