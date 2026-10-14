-- 0020_task_resilience: I-2 crash-resume cap. tasks.crash_resumes counts
-- ONLY the resume scan's own "no live worker, nothing parked/blocked"
-- re-dispatches (kahyad/internal/task.Resume.enqueueResume) - never the
-- cloud-park redispatches that kahyad/internal/task.Machine.Transition
-- already counts in tasks.attempts (a parked task legitimately retries
-- for up to cloud_retry_give_up_after, far more than any crash cap).
-- Past config task_max_crash_resumes the resume scan moves the task to
-- blocked_user instead of enqueueing it again.
-- Reset to 0 by `kahya task resolve <id> --retry` (an explicit human
-- decision starts a fresh budget).
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header).

-- +goose Up
ALTER TABLE tasks ADD COLUMN crash_resumes INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE tasks DROP COLUMN crash_resumes;
