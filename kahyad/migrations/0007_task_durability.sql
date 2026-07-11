-- 0007_task_durability: the W4-02 task durability state machine +
-- idempotency/receipt lifecycle + outbox redelivery (HANDOFF S6 W4 flag:
-- "idempotency/makbuz semantigi (intent -> executing -> receipt);
-- makbuzsuz executing'de yalniz W1 oto-tekrar, W2/W3 asla"; S4 IPC flag:
-- "W4 oturum devami session_id ile").
--
-- tasks.status is a NEW column, deliberately separate from the pre-
-- existing tasks.state (free-form SSE/outcome label kahyad/internal/
-- server/task.go already writes: "running"/"done"/"error"/
-- "paused_budget" - unrelated bookkeeping this migration does not touch).
-- status is the durability lifecycle kahyad/internal/task.Machine
-- exclusively owns: intent -> executing -> {done, failed, blocked_user,
-- bekliyor-yeniden-deneme, user_halted}, plus the two escape edges
-- (bekliyor-yeniden-deneme/blocked_user) -> user_halted so a later W6-03
-- halt can always reach a parked/blocked task, not just an executing one.
-- 'bekliyor-yeniden-deneme' is POPULATED by W4-04 (cloud-error taxonomy);
-- 'user_halted' SEMANTICS are W6-03 (kill process-group + invalidate
-- approvals) - both enum values must exist now so those two later tasks
-- are pure logic against an already-shaped column, per the task spec.
-- DEFAULT 'intent' keeps every pre-W4-02 InsertTask call (which does not
-- set this column) inserting a valid row rather than violating NOT NULL.
--
-- tool_calls is the per-side-effectful-call receipt ledger: ONE row's
-- status walks intent -> executing -> {receipt, failed} as
-- kahyad/internal/task.Receipts drives a single W1/W2/W3 tool execution
-- (R-class calls get no row at all - no side effect to protect). seq lets
-- the SAME (task_id, tool_name, args_hash) triple appear more than once
-- across separate attempts (a receipt-less row that a resume scan marks
-- 'failed' before requeueing a genuinely NEW execution attempt, which
-- inserts a fresh row at seq+1) while the replay lookup (kahyad/internal/
-- task.Receipts.Execute, "status = 'receipt'") only ever matches the
-- attempt that actually completed - never re-executing once any attempt
-- for that exact triple has a durable receipt.
--
-- outbox gains available_at/lease_until/attempts (W12-02's 0001 schema
-- shipped id/trace_id/kind/payload/dispatched_at only - the redelivery
-- dispatcher (kahyad/internal/outbox) is this task's own deliverable, so
-- its lease columns are added here rather than backdated into 0001).
--
-- NOTE: keep this file plain ASCII (see 0001_init_schema.sql's header -
-- sqlc's sqlite parser silently corrupts every generated query in the
-- package on a single non-ASCII byte in any schema/query file it reads).

-- +goose Up
ALTER TABLE tasks ADD COLUMN status TEXT NOT NULL DEFAULT 'intent'
    CHECK (status IN ('intent', 'executing', 'bekliyor-yeniden-deneme', 'blocked_user', 'user_halted', 'done', 'failed'));
ALTER TABLE tasks ADD COLUMN next_retry_at TEXT;
ALTER TABLE tasks ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

CREATE TABLE tool_calls (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id           TEXT NOT NULL REFERENCES tasks(id),
    seq               INTEGER NOT NULL,
    tool_name         TEXT NOT NULL,
    class             TEXT NOT NULL CHECK (class IN ('R', 'W1', 'W2', 'W3')),
    args_hash         TEXT NOT NULL,
    approval_token_id TEXT,
    status            TEXT NOT NULL CHECK (status IN ('intent', 'executing', 'receipt', 'failed')),
    receipt_json      TEXT,
    started_at        TEXT,
    finished_at       TEXT,
    created_at        TEXT NOT NULL,
    UNIQUE (task_id, tool_name, args_hash, seq)
);

CREATE INDEX idx_tool_calls_task_id ON tool_calls(task_id);
-- Idempotent-replay lookup index (kahyad/internal/task.Receipts.Execute):
-- "does a status='receipt' row already exist for (task_id, tool_name,
-- args_hash)?", checked before every genuine execution attempt.
CREATE INDEX idx_tool_calls_replay ON tool_calls(task_id, tool_name, args_hash, status);

ALTER TABLE outbox ADD COLUMN available_at TEXT;
ALTER TABLE outbox ADD COLUMN lease_until TEXT;
ALTER TABLE outbox ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_outbox_available ON outbox(dispatched_at, available_at);

-- +goose Down
DROP INDEX IF EXISTS idx_outbox_available;
ALTER TABLE outbox DROP COLUMN attempts;
ALTER TABLE outbox DROP COLUMN lease_until;
ALTER TABLE outbox DROP COLUMN available_at;

DROP INDEX IF EXISTS idx_tool_calls_replay;
DROP INDEX IF EXISTS idx_tool_calls_task_id;
DROP TABLE IF EXISTS tool_calls;

ALTER TABLE tasks DROP COLUMN attempts;
ALTER TABLE tasks DROP COLUMN next_retry_at;
ALTER TABLE tasks DROP COLUMN status;
