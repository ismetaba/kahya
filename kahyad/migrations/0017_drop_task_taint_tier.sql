-- +goose Up
-- W4-03 follow-up: drop the vestigial tasks.taint_tier column. It was written
-- at task creation (always a literal 'untrusted'/'clean') and SELECTed into the
-- Task model, but is NEVER read for any taint decision anywhere in the codebase
-- (grep-confirmed). The authoritative, monotonic session taint lives in the
-- session_taint table (migration 0009), keyed by session_id and enforced by
-- kahyad/internal/taint.Tracker + kahyad/internal/policy's denyIfTainted - that
-- is the layer every W-tool decision consults. tasks.taint_tier was dead
-- storage that made the row look like it drove taint when it never did;
-- removing it keeps the schema honest. DROP COLUMN is safe here: the column is
-- a plain TEXT with a default, carried by no index/view/constraint.
ALTER TABLE tasks DROP COLUMN taint_tier;

-- +goose Down
ALTER TABLE tasks ADD COLUMN taint_tier TEXT NOT NULL DEFAULT 'untrusted';
