-- 0022_undo_windows_per_op: B-8 fix - one undo_windows row per EXECUTED
-- W1 operation instead of one per (task_id, tool, trace_id).
--
-- op_id is the approval_tokens.token_hash (sha256 hex of the 32 random
-- token bytes - never the raw token) of the one-time token the operation
-- consumed. kahyad/internal/policy.Engine.ConsumeTokenOp opens the row
-- at consume time, so an op_id is unique per executed side effect by
-- construction (ConsumeApprovalToken is an atomic single-use UPDATE).
-- Engine.Check / Engine.Approve / the B-1 grant redemption no longer open
-- windows at all.
--
-- scope is the token's REAL bound scope (approval_tokens.scope), so
-- TriggerUndo demotes the real (tool, class, scope) triple instead of the
-- documented 'global' simplification.
--
-- Legacy rows (opened before this migration, including the B-1
-- redemption-time windows of 0018) keep op_id = '' and scope = 'global';
-- `kahya undo` reports them as no_record and they expire through the
-- normal sweep. The partial UNIQUE index skips them.
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header).

-- +goose Up
ALTER TABLE undo_windows ADD COLUMN op_id TEXT NOT NULL DEFAULT '';
ALTER TABLE undo_windows ADD COLUMN scope TEXT NOT NULL DEFAULT 'global';
CREATE UNIQUE INDEX idx_undo_windows_op_id ON undo_windows(op_id) WHERE op_id <> '';

-- +goose Down
DROP INDEX IF EXISTS idx_undo_windows_op_id;
ALTER TABLE undo_windows DROP COLUMN scope;
ALTER TABLE undo_windows DROP COLUMN op_id;
