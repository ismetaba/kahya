-- 0011_consolidation: W5-02 nightly consolidation's own addition to
-- episodes (HANDOFF S5 memory #4 flag: "90+ gun sicak pencere +
-- ayrinti-atomu ... Sogutmadan once sayi/tarih/alinti/karar/soz'ler
-- yapilandirilmis olgulara terfi").
--
-- cooled_at is NULL until the hot-window pass (kahyad/internal/
-- consolidation/hotwindow.go) has promoted an episode's detail atoms to
-- agent_derived candidate facts; it is stamped (RFC3339 UTC, matching
-- every other *_at column in this schema) exactly once, AFTER promotion,
-- never before - so a crash/restart between promotion and stamping simply
-- re-promotes on the next nightly run rather than silently skipping an
-- episode's atoms forever (idempotent: InsertFact rows are evidence-
-- keyed, a duplicate promotion pass is a correctness nit, not a security
-- one, and W5-04 owns fact-merge correctness).
--
-- Plain ALTER TABLE (no FTS5/vec0 syntax) - like 0003/0004/0005/0006/0007,
-- this file IS added to sqlc.yaml's schema list. Keep this file plain
-- ASCII (see 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE episodes ADD COLUMN cooled_at TEXT;

CREATE INDEX idx_episodes_cooled_at ON episodes(cooled_at);

-- +goose Down
DROP INDEX IF EXISTS idx_episodes_cooled_at;
ALTER TABLE episodes DROP COLUMN cooled_at;
