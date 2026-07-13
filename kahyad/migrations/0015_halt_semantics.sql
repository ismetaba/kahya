-- 0015_halt_semantics: the W6-03 emergency-halt (⌥⎋) columns (HANDOFF §6
-- W6 flag: "Hammerspoon'dan kahyad'a 'halt' IPC -> worker process-group'u
-- + ilgili Docker konteynerleri oldurulur, gorev terminal user_halted
-- durumuna yazilir (session-resume ve outbox retry'dan kalici haric),
-- bekleyen tum onaylar gecersiz kilinir").
--
-- tasks.worker_pgid persists the spawned worker's process-group id
-- (kahyad/internal/spawn.Run always starts the worker as its own new
-- process-group leader via Setpgid, so pid == pgid) ALONGSIDE the
-- existing in-memory kahyad/internal/task.LiveRegistry - macOS has no
-- PDEATHSIG, so a worker spawned before a kahyad crash/restart keeps
-- running as an orphan with an now-EMPTY in-memory registry; the halt
-- executor falls back to this column so ⌥⎋ is still a real stop
-- immediately after a daemon restart (W6-03 task spec step 1). NULL until
-- the worker's OnStart callback fires (or for a task that never spawned a
-- worker at all, e.g. briefing/consolidation sessions).
--
-- tasks.halted_at is stamped by the halt executor in the SAME step that
-- transitions tasks.status to 'user_halted' (kahyad/internal/task.
-- StatusUserHalted, already a legal destination from executing/
-- bekliyor-yeniden-deneme/blocked_user as of migrations/0007's own doc
-- comment) - a plain audit timestamp, not itself consulted by any
-- exclusion guard (tasks.status alone is authoritative for that).
--
-- outbox.task_id back-links every OutboxKindTaskResume row
-- (kahyad/internal/task.OutboxTaskResumePayload already carries task_id
-- inside its JSON payload; this column is the SAME value, promoted to a
-- real, queryable column) to the task it would redeliver - the halt
-- executor's own "cancel undelivered outbox rows" step (W6-03 task spec
-- step 3.4) and the ListDueOutboxRows defense-in-depth join (step 5)
-- both need this to reach the owning tasks row directly, without parsing
-- payload JSON in SQL. NULL for any future outbox kind that is not
-- task-scoped (none exist yet) - such a row is simply unaffected by both
-- of those task_id-keyed guards, exactly as before this migration.
--
-- outbox.canceled_at is the halt executor's own per-row marker ("cancel
-- the task's undelivered outbox rows" - W6-03 task spec step 3.4): set
-- once, only for rows still undelivered (dispatched_at IS NULL) at halt
-- time, and checked by ListDueOutboxRows alongside (not instead of) the
-- separate tasks.status != 'user_halted' join guard added to that same
-- query - two independent reasons the SAME row can never be redelivered,
-- deliberately redundant (defense in depth, W6-03 task spec step 5).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
ALTER TABLE tasks ADD COLUMN worker_pgid INTEGER;
ALTER TABLE tasks ADD COLUMN halted_at TEXT;

ALTER TABLE outbox ADD COLUMN task_id TEXT;
ALTER TABLE outbox ADD COLUMN canceled_at TEXT;

-- +goose Down
ALTER TABLE outbox DROP COLUMN canceled_at;
ALTER TABLE outbox DROP COLUMN task_id;

ALTER TABLE tasks DROP COLUMN halted_at;
ALTER TABLE tasks DROP COLUMN worker_pgid;
