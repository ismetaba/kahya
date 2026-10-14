-- 0018_approval_grants: B-1 approval loop. approval_tokens gains the grant
-- lineage the tool-side binding Engine.Check needs to REDEEM a human
-- approval (kahyad/internal/policy/engine.go redeemApprovedGrant):
--   grant_kind 'auto'      = Engine.Check's own ladder auto-allow token,
--   grant_kind 'approval'  = Engine.Approve's grant (redeemable exactly once),
--   grant_kind 'exchanged' = the fresh token minted AT redemption (never
--                            redeemable again; consumed by ConsumeToken).
-- pending_approval_id links an 'approval'/'exchanged' row back to the
-- pending_approvals row the human answered.
-- pending_approvals.decision records the human outcome so the server-side
-- waiter and `kahya approvals` can tell approved/denied/invalidated apart
-- (consumed_at alone could not).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
ALTER TABLE approval_tokens ADD COLUMN grant_kind TEXT NOT NULL DEFAULT 'auto'
    CHECK (grant_kind IN ('auto', 'approval', 'exchanged'));
ALTER TABLE approval_tokens ADD COLUMN pending_approval_id TEXT;
CREATE INDEX idx_approval_tokens_grant
    ON approval_tokens(task_id, tool, scope, approved_bytes_hash, grant_kind);

ALTER TABLE pending_approvals ADD COLUMN decision TEXT
    CHECK (decision IS NULL OR decision IN ('approved', 'denied', 'invalidated'));

-- +goose Down
ALTER TABLE pending_approvals DROP COLUMN decision;
DROP INDEX IF EXISTS idx_approval_tokens_grant;
ALTER TABLE approval_tokens DROP COLUMN pending_approval_id;
ALTER TABLE approval_tokens DROP COLUMN grant_kind;
