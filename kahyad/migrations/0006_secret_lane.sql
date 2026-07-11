-- 0006_secret_lane: the W3-08 secret-lane routing branch's own addition to
-- tasks (HANDOFF S4 routing ordering invariant + S4 memory pressure flag).
-- lane is per-TASK and STICKY (task spec gotcha: "once a task is lane:
-- secret, it stays secret for its lifetime, including resume") -
-- kahyad/internal/secretlane.Escalate enforces the "only ever widens,
-- never downgrades" rule in Go (the same "monotonic, enforced in Go, not
-- a SQL constraint" posture tasks.taint_tier already documents in
-- 0001_init_schema.sql). secret_category is nullable: 'finans'|'saglik'|
-- 'kimlik'|'none', NULL for a task never classified at all (pre-W3-08 rows,
-- or any lane='normal' row this classifier never needed to touch).
--
-- kahyad/internal/secretlane.NewProxyBackstopHook (the W12-08 proxy
-- chokepoint enforcement) reads lane by task_id on EVERY forwarded
-- request - this is the "task registry by trace_id/task_id" the task spec
-- names.
--
-- Plain ALTER TABLE (no FTS5/vec0 syntax) - like 0003/0004/0005, this file
-- IS added to sqlc.yaml's schema list. Keep this file plain ASCII (see
-- 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE tasks ADD COLUMN lane TEXT NOT NULL DEFAULT 'normal' CHECK (lane IN ('normal', 'secret'));
ALTER TABLE tasks ADD COLUMN secret_category TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN secret_category;
ALTER TABLE tasks DROP COLUMN lane;
