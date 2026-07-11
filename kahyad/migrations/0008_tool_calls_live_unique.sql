-- 0008_tool_calls_live_unique: closes the W4-02 concurrent-Execute
-- double-effect hole (task durability BLOCKER 1).
--
-- Before this migration, kahyad/internal/task.Receipts.Execute's
-- idempotent-replay check (GetReceiptToolCall), NextToolCallSeq, and
-- InsertToolCallIntent were three separate, un-transacted statements: two
-- concurrent Execute() calls for the IDENTICAL (task_id, tool_name,
-- args_hash) triple could both pass the replay guard (neither sees a
-- 'receipt' row yet), both compute a seq, and both insert an 'intent' row
-- - tool_calls' existing UNIQUE(task_id, tool_name, args_hash, seq)
-- constraint does not stop this, because NextToolCallSeq itself raced and
-- handed each caller a DIFFERENT seq. Both callers then run the real
-- side-effectful effect: a genuine double execution.
--
-- This index makes "at most one LIVE attempt per (task_id, tool_name,
-- args_hash), regardless of seq" a DATABASE-enforced invariant, not just
-- an application-level convention: a second InsertToolCallIntent for a
-- key that already has an 'intent'/'executing'/'receipt' row FAILS
-- (SQLite unique-constraint violation) instead of silently succeeding at
-- a different seq. kahyad/internal/task.Receipts.Execute (BLOCKER 1 fix)
-- catches that violation, re-reads the live row, and either replays its
-- receipt or waits for it to resolve - it never runs a second real
-- effect. 'receipt' is deliberately included in the WHERE list even
-- though a receipted call is normally already caught by the replay
-- lookup before any insert is attempted: it is the belt to that
-- braces for the same crash-safety reason every other partial-unique
-- index in this schema is - a defensive invariant that holds even if the
-- application-level check is ever bypassed or racy, not merely a
-- documentation comment.
--
-- The existing table-level UNIQUE(task_id, tool_name, args_hash, seq) is
-- left untouched (0007_task_durability.sql) - this migration ADDS a
-- second, narrower partial index alongside it; it does not replace
-- anything.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
CREATE UNIQUE INDEX idx_tool_calls_live_unique
    ON tool_calls(task_id, tool_name, args_hash)
    WHERE status IN ('intent', 'executing', 'receipt');

-- +goose Down
DROP INDEX IF EXISTS idx_tool_calls_live_unique;
