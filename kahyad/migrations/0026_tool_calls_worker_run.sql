-- 0026_tool_calls_worker_run: receipts replay only on resume (FIX-FINAL
-- receipt-replay-per-run, owner decision 2026-10-16).
--
-- Before this migration the idempotent-replay lookup answered ANY
-- status='receipt' row of the same (task_id, tool_name, args_hash) for the
-- task's whole life, and idx_tool_calls_live_unique (0008) allowed only one
-- live (intent/executing/receipt) row per key. So a legitimate repeat of an
-- identical call inside ONE live worker run (fs_write X, then Y, then X
-- again; re-running the same shell_docker test script after an edit; a
-- second `git status`) was answered from the first call's stale receipt and
-- never ran.
--
-- A task's worker RUN (incarnation) is counted kahyad-side:
--
--   tasks.worker_run - 0 until the task's first worker spawn; every spawn
--   of a worker for the task (the first one in POST /v1/task, and every
--   outbox redispatch: resume after a crash or an orphan sweep, cloud-retry
--   redispatch, `kahya task resolve --retry`) increments it BEFORE the
--   worker starts, so the first spawn is run 1. It is never taken from the
--   worker.
--
--   tool_calls.run - the run that made the attempt (kahyad resolves it per
--   tool call from tasks.worker_run, pinned once per call).
--
--   tool_calls.replayed_run - the latest run that consumed this receipt by
--   replaying it (0 = never). Runs only grow, so "consumed by the current
--   run" is exactly replayed_run = current run.
--
-- A receipt is replayed only for a run LATER than the one that wrote it,
-- and at most once per run: after a crash following X, Y, X the resumed run
-- replays those three and a 4th X executes. Inside the current run an
-- identical call executes normally (a new row, a normal policy decision).
--
-- idx_tool_calls_live_unique is redefined to cover only the IN-FLIGHT
-- statuses (intent, executing): many receipts of one key are now legal (one
-- per executed call), but two in-flight attempts of one key still are not -
-- the BLOCKER 1 guarantee (no two concurrent executions of the identical
-- call) stays DB-enforced.
--
-- Every pre-0026 row defaults to run 0 (and every task to worker_run 0):
-- the task's next spawn is run 1, so each pre-0026 receipt can be replayed
-- once by the resumed run - the pre-0026 resume semantics for those rows.
--
-- Down restores 0008's index; it fails when a key already has more than one
-- receipt (a live repeat happened under 0026) - roll back data first.
--
-- Plain ALTER TABLE / DROP INDEX / CREATE INDEX only - this file IS in
-- sqlc.yaml's schema list. Keep this file plain ASCII (see
-- 0001_init_schema.sql's header comment for why).

-- +goose Up
ALTER TABLE tasks ADD COLUMN worker_run INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN run INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN replayed_run INTEGER NOT NULL DEFAULT 0;

DROP INDEX IF EXISTS idx_tool_calls_live_unique;
CREATE UNIQUE INDEX idx_tool_calls_live_unique
    ON tool_calls(task_id, tool_name, args_hash)
    WHERE status IN ('intent', 'executing');

-- +goose Down
DROP INDEX IF EXISTS idx_tool_calls_live_unique;
CREATE UNIQUE INDEX idx_tool_calls_live_unique
    ON tool_calls(task_id, tool_name, args_hash)
    WHERE status IN ('intent', 'executing', 'receipt');
ALTER TABLE tool_calls DROP COLUMN replayed_run;
ALTER TABLE tool_calls DROP COLUMN run;
ALTER TABLE tasks DROP COLUMN worker_run;
