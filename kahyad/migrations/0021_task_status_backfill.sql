-- 0021_task_status_backfill: I-14. Before I-14, local/secret-lane
-- (handleSecretLaneTask), briefing and reader task rows never entered the
-- W4-02 status machine and sat at the column DEFAULT 'intent' forever,
-- so every halt swept them. Map the rows whose free-form state already
-- says how they ended; leave genuinely unknown rows alone (an old row
-- still at state 'running' is halted, legally, by the new
-- intent -> user_halted edge on the next halt-all - a one-time effect).
--
-- Data only: no schema change, so this file is intentionally NOT in
-- sqlc.yaml. Down is a no-op (the old 'intent' values carried no
-- information worth restoring).
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header).

-- +goose Up
UPDATE tasks SET status = 'done' WHERE status = 'intent' AND state = 'done';
UPDATE tasks SET status = 'failed' WHERE status = 'intent' AND state = 'error';

-- +goose Down
SELECT 1;
