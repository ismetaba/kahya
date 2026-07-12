-- Starter query set for W12-02 (HANDOFF S4: Go + sqlc-generated queries).
-- Later tasks add more queries to this file as they need them; sqlc
-- regenerates the whole package from the union of every *.sql file here.

-- name: InsertEvent :one
INSERT INTO events (trace_id, ts, kind, payload, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, trace_id, ts, kind, payload, created_at;

-- name: ListEventsByTrace :many
SELECT id, trace_id, ts, kind, payload, created_at
FROM events
WHERE trace_id = ?
ORDER BY id ASC;

-- name: ListEventsByKind :many
-- W12-08 (anthproxy cost governor): boot-time rebuild reads every
-- historical event of one kind (e.g. 'model_call') and replays it into
-- the in-memory governor totals - kahyad/internal/anthproxy stays
-- store-agnostic (it never imports this package); main.go converts each
-- row into an anthproxy.BootEvent.
SELECT id, trace_id, ts, kind, payload, created_at
FROM events
WHERE kind = ?
ORDER BY id ASC;

-- name: InsertTask :one
-- lane/secret_category (W3-08): the caller ALREADY knows this task's
-- secret-lane verdict before this row is ever created (kahyad/internal/
-- server's POST /v1/task handler runs kahyad/internal/secretlane's
-- classifier BEFORE calling InsertTask - the ordering invariant, HANDOFF
-- S4 flag - so there is no window where a task row exists with an
-- unclassified lane). The RETURNING clause lists every physical column
-- (including status/next_retry_at/attempts, W4-02's 0007_task_durability
-- ALTER TABLEs - left OUT of the INSERT column/VALUES list itself, so
-- every existing caller keeps inserting a row that takes their DEFAULTs
-- unchanged: status='intent', attempts=0, next_retry_at=NULL) so sqlc
-- reuses the existing Task model type here rather than generating a
-- second, differently-ordered InsertTaskRow type.
INSERT INTO tasks (id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at, lane, secret_category)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts;

-- name: UpdateTaskState :exec
UPDATE tasks
SET state = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateTaskSession :exec
-- Persists a worker-reported session_id onto the task row (W12-07 step 3:
-- kahyad "persists session_id onto the task row" as the worker's
-- {"type":"session",...} stdout line arrives). Session RESUME itself
-- (using a stored session_id to continue a LATER task) is W4-02; this
-- query only records the value as it arrives.
UPDATE tasks
SET session_id = ?, updated_at = ?
WHERE id = ?;

-- W3-08 (secret-lane routing) queries below: lane/secret_category are
-- STICKY (kahyad/internal/secretlane.Escalate enforces "only ever widens,
-- never downgrades" in Go, not SQL - see 0006_secret_lane.sql's own doc
-- comment). kahyad/internal/secretlane.NewProxyBackstopHook (the W12-08
-- proxy chokepoint) is GetTaskLane's other caller, consulted on EVERY
-- forwarded model-call request.

-- name: SetTaskLane :exec
UPDATE tasks
SET lane = ?, secret_category = ?, updated_at = ?
WHERE id = ?;

-- name: GetTaskLane :one
SELECT lane, secret_category FROM tasks WHERE id = ?;

-- name: GetTaskBySession :one
-- Sessions are not currently guaranteed to map to exactly one task row
-- (resume/retry may append more), so this returns the most recently
-- updated task for the session.
SELECT id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at
FROM tasks
WHERE session_id = ?
ORDER BY updated_at DESC
LIMIT 1;

-- name: GetTaskSessionByTrace :one
-- W4-03 BLOCKER 1+2 fix: the by-trace_id half of the server-side taint
-- resolver (kahyad/internal/policy.StoreSessionResolver) - a policy
-- decision must resolve WHICH session a request belongs to from the
-- request's own trace_id/task_id correlation, never from a caller-
-- supplied session_id (untrusted; on POST /v1/mcp there is no session_id
-- on the wire at all - see mcp.go's policyGateMiddleware). Mirrors
-- GetTaskBySession's own "not guaranteed unique, most-recently-updated
-- wins" note: a resumed/retried task can share one trace_id across more
-- than one tasks row. NULL session_id (no session_started yet for this
-- task) comes back as a NULL/invalid value, not an error - the resolver
-- itself treats that as "unresolved" (fail-closed), not a query failure.
SELECT session_id FROM tasks
WHERE trace_id = ?
ORDER BY updated_at DESC
LIMIT 1;

-- name: InsertEpisode :one
INSERT INTO episodes (source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at;

-- name: InsertChunk :one
INSERT INTO chunks (episode_id, seq, text, content_hash, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, episode_id, seq, text, content_hash, created_at;

-- name: DeleteChunksByEpisode :exec
DELETE FROM chunks WHERE episode_id = ?;

-- name: GetEpisodeByPath :one
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at
FROM episodes
WHERE source_path = ?
LIMIT 1;

-- W12-04 (corpus indexer) queries below. GetEpisodeByPath above does not
-- filter by source, which is fine for callers that only ever use one
-- source, but the indexer must scope its hash-compare lookup to
-- source='memory_file' specifically (task spec step 3), so it gets its own
-- query rather than overloading GetEpisodeByPath's signature.

-- name: GetEpisodeBySourceAndPath :one
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at
FROM episodes
WHERE source = ? AND source_path = ?
LIMIT 1;

-- name: UpdateEpisodeContent :exec
-- Upserts (update half) an existing memory_file episode in place on
-- new/changed content: same id, fresh hash/tier, status forced back to
-- 'active' (covers the resurrect-a-deleted-file case, not just plain edits).
UPDATE episodes
SET source_hash = ?, source_tier = ?, status = ?
WHERE id = ?;

-- name: MarkEpisodeDeleted :exec
UPDATE episodes
SET status = 'deleted'
WHERE id = ?;

-- name: ListChunkIDsByEpisode :many
SELECT id FROM chunks WHERE episode_id = ? ORDER BY seq ASC;

-- name: ListActiveMemoryFileEpisodes :many
SELECT id, source_path FROM episodes
WHERE source = 'memory_file' AND status = 'active';

-- W3-02 (autonomy ladder engine) queries below: autonomy_state,
-- approval_tokens, undo_windows. See migrations/0003_autonomy_policy.sql
-- for the schema and kahyad/internal/policy/engine.go + tokens.go for the
-- only two callers.

-- name: GetAutonomyState :one
SELECT tool, class, scope, level, consecutive_approvals, updated_at
FROM autonomy_state
WHERE tool = ? AND class = ? AND scope = ?;

-- name: ListAutonomyState :many
SELECT tool, class, scope, level, consecutive_approvals, updated_at
FROM autonomy_state
ORDER BY tool, class, scope;

-- name: InsertAutonomyState :exec
INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: UpdateAutonomyState :execrows
-- Update-half of an application-level upsert (kahyad/internal/policy
-- calls this first; a 0-rows-affected result means no row exists yet, so
-- it falls back to InsertAutonomyState - the same "upsert (update half)"
-- pattern UpdateEpisodeContent above already uses in this file).
UPDATE autonomy_state
SET level = ?, consecutive_approvals = ?, updated_at = ?
WHERE tool = ? AND class = ? AND scope = ?;

-- name: InsertApprovalToken :exec
-- consumed_at starts NULL (a literal, not a param - see
-- ConsumeApprovalToken below for the only statement that ever sets it).
-- class/scope persist the token's REAL bound identity (post-security-
-- review amendment) so a consume-failure demotion can target this exact
-- triple, recovered by token_hash - never whatever (tool,class,scope) the
-- /policy/consume-token caller happens to claim.
INSERT INTO approval_tokens (token_hash, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL);

-- name: GetApprovalToken :one
SELECT token_hash, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at
FROM approval_tokens
WHERE token_hash = ?;

-- name: ConsumeApprovalToken :execrows
-- The single atomic single-use guarantee (HANDOFF S5 safety #5): only the
-- FIRST caller to run this UPDATE against a given token_hash ever affects
-- a row (consumed_at IS NULL is only ever true once); every later call
-- against the same token_hash - correct bytes or not - affects 0 rows and
-- kahyad/internal/policy/tokens.go treats that as a replay/unknown-token
-- failure. Bytes-hash/expiry comparison happens in Go, in a follow-up
-- GetApprovalToken call, AFTER this UPDATE has already burned the token -
-- so even a wrong-hash first presentation consumes it (no multi-guess
-- window against a live token).
UPDATE approval_tokens
SET consumed_at = ?
WHERE token_hash = ? AND consumed_at IS NULL;

-- name: InsertUndoWindow :one
INSERT INTO undo_windows (task_id, tool, trace_id, opened_at, deadline, state)
VALUES (?, ?, ?, ?, ?, 'open')
RETURNING id, task_id, tool, trace_id, opened_at, deadline, state;

-- name: GetOpenUndoWindowByTrace :one
-- Sessions/tasks are not guaranteed to open exactly one undo window per
-- trace_id, so this returns the most recently opened OPEN one - the same
-- "most recent wins" convention GetTaskBySession above already uses.
SELECT id, task_id, tool, trace_id, opened_at, deadline, state
FROM undo_windows
WHERE trace_id = ? AND state = 'open'
ORDER BY opened_at DESC
LIMIT 1;

-- name: GetOpenUndoWindowByTaskToolTrace :one
-- Idempotent-open lookup (post-security-review amendment): Engine.Check's
-- W1 auto-allow path (and Approve's W1 bookkeeping) call this FIRST and
-- reuse an already-OPEN window for the same (task_id, tool, trace_id)
-- instead of opening a second one on a retried call - a retry must never
-- leave multiple simultaneously-open undo windows for the same action.
SELECT id, task_id, tool, trace_id, opened_at, deadline, state
FROM undo_windows
WHERE task_id = ? AND tool = ? AND trace_id = ? AND state = 'open'
ORDER BY opened_at DESC
LIMIT 1;

-- name: ListOpenUndoWindows :many
SELECT id, task_id, tool, trace_id, opened_at, deadline, state
FROM undo_windows
WHERE state = 'open';

-- name: SetUndoWindowState :exec
UPDATE undo_windows
SET state = ?
WHERE id = ?;

-- Post-security-review amendment: pending_approvals queries below back the
-- server-issued, single-use pending_approval_id (see
-- migrations/0003_autonomy_policy.sql's doc comment - a NEEDS_APPROVAL
-- decision used to hand back an unsigned, caller-decodable blob; it is now
-- an opaque random id bound to a DB row only kahyad/internal/policy/
-- engine.go writes/reads).

-- name: InsertPendingApproval :exec
-- consumed_at starts NULL (a literal, not a param - see
-- ConsumePendingApproval below for the only statement that ever sets it).
-- tool_input (W3-06) persists the EXACT bytes Engine.Check received, so
-- `kahya approvals`/`kahya approve <id>` can render a real WYSIWYE diff,
-- not just prove after the fact (via approved_bytes_hash) that nothing
-- changed. Listed last (matching the column's physical ALTER-TABLE-added
-- position - see 0005_pending_approval_payload.sql) so GetPendingApproval/
-- ListUnconsumedPendingApprovals below reuse the PendingApproval model
-- type directly instead of sqlc emitting a separate "Row" type for a
-- differently-ordered column list.
INSERT INTO pending_approvals (id, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at, tool_input)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?);

-- name: GetPendingApproval :one
SELECT id, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at, tool_input
FROM pending_approvals
WHERE id = ?;

-- name: ListUnconsumedPendingApprovals :many
-- W3-06 `kahya approvals`: every not-yet-consumed row, oldest first.
-- Expiry is checked in Go (kahyad/internal/policy.Engine.ListPendingApprovals),
-- mirroring getValidPendingApproval's own time.Parse+time.After check,
-- rather than comparing RFC3339Nano strings in SQL (that format's
-- trailing-zero-trimmed fractional seconds do NOT always sort
-- lexicographically in timestamp order).
SELECT id, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at, tool_input
FROM pending_approvals
WHERE consumed_at IS NULL
ORDER BY minted_at ASC;

-- name: ConsumePendingApproval :execrows
-- The single atomic single-use guarantee (the same "UPDATE ... WHERE x IS
-- NULL" pattern ConsumeApprovalToken above already uses for one-time
-- approval tokens): only the FIRST Engine.Approve/Deny call against a
-- given pending_approval_id ever affects a row; a second call against the
-- same id - Approve or Deny, forged or genuine - affects 0 rows, which
-- kahyad/internal/policy/engine.go treats as already-used/rejects, minting
-- no token and performing no bookkeeping a second time.
UPDATE pending_approvals
SET consumed_at = ?
WHERE id = ? AND consumed_at IS NULL;

-- W3-05 (egress proxy) queries below: egress_budget persists each host's
-- daily byte counter across restarts. See
-- migrations/0004_egress_budget.sql for the schema and
-- kahyad/internal/egress/budget.go for the only caller.

-- name: GetEgressBudget :one
SELECT host, day, bytes FROM egress_budget WHERE host = ? AND day = ?;

-- name: InsertEgressBudget :exec
INSERT INTO egress_budget (host, day, bytes) VALUES (?, ?, ?);

-- name: IncrementEgressBudget :execrows
-- Update-half of an application-level upsert (the same "upsert (update
-- half) then fall back to Insert on 0 rows" pattern UpdateAutonomyState/
-- UpdateEpisodeContent above already use in this file) - the common case
-- once a (host, day) row already exists.
UPDATE egress_budget
SET bytes = bytes + ?
WHERE host = ? AND day = ?;

-- W4-02 (task durability state machine + receipts + outbox redelivery)
-- queries below. See migrations/0007_task_durability.sql for the schema;
-- kahyad/internal/task (machine.go/receipts.go/resume.go) and
-- kahyad/internal/outbox (dispatcher.go) are the only callers.

-- name: GetTaskByID :one
-- Column list matches tasks' own physical column order exactly, so sqlc
-- reuses the existing Task model type here rather than emitting a second,
-- differently-ordered Row type (the same convention InsertTask's own doc
-- comment already established for lane/secret_category).
SELECT id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts
FROM tasks
WHERE id = ?;

-- name: SetTaskStatus :execrows
-- The ONLY writer of tasks.status is kahyad/internal/task.Machine.Transition
-- - every status change is preceded by that function's own legal-transition
-- check and followed by a task.transition (or task.illegal_transition)
-- ledger event; this query performs no validation of its own.
--
-- task durability BLOCKER 3 fix: the WHERE clause's own "status = ?" (the
-- FROM status Machine.Transition just read via GetTaskByID) makes this an
-- atomic compare-and-set, not a blind write - two concurrent Transition
-- calls racing from the SAME stale 'from' read can now only ever have ONE
-- of them actually affect a row (rows-affected 1); the loser affects 0
-- rows (its 'from' no longer matches - some other transition already won)
-- and Machine.Transition treats that as a lost race, never silently
-- overwriting whatever the winner just wrote (no more last-write-wins).
UPDATE tasks
SET status = ?, updated_at = ?
WHERE id = ? AND status = ?;

-- name: IncrementTaskAttempts :one
-- Bumps tasks.attempts by one and returns the new value - used both by
-- Machine.Transition (every fresh dispatch INTO 'executing') and by the
-- resume scan's within-cap W1 receipt-less retry path (which re-dispatches
-- without any status change, since the task never leaves 'executing').
UPDATE tasks
SET attempts = attempts + 1, updated_at = ?
WHERE id = ?
RETURNING attempts;

-- name: SetTaskNextRetry :exec
-- W4-04: writes tasks.next_retry_at when kahyad/internal/task.CloudRetry
-- parks a task in 'bekliyor-yeniden-deneme' (the status change itself
-- goes through Machine.Transition/SetTaskStatus, same as every other
-- transition - this query ONLY ever touches next_retry_at, so a caller
-- always calls both, never this alone).
UPDATE tasks
SET next_retry_at = ?, updated_at = ?
WHERE id = ?;

-- name: ListExecutingTasks :many
-- The resume scan's candidate set (kahyad startup + periodic tick):
-- every task currently recorded as 'executing'. kahyad/internal/task's
-- own LiveRegistry then filters this down to the ones with NO live worker
-- PID - a live task is simply skipped (the daemon itself is still running
-- it).
SELECT id, trace_id, session_id, state, taint_tier, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts
FROM tasks
WHERE status = 'executing'
ORDER BY updated_at ASC;

-- name: InsertToolCallIntent :one
-- Step 1 of the intent -> executing -> {receipt, failed} lifecycle
-- (kahyad/internal/task.Receipts.Execute): one row per side-effectful
-- (W1/W2/W3) tool-call ATTEMPT - seq lets the SAME (task_id, tool_name,
-- args_hash) triple be re-attempted (a fresh row at a higher seq) after an
-- earlier attempt at that exact triple was marked 'failed' by the resume
-- scan, while the idempotent-replay lookup (GetReceiptToolCall below)
-- only ever matches an attempt that reached 'receipt'.
INSERT INTO tool_calls (task_id, seq, tool_name, class, args_hash, approval_token_id, status, receipt_json, started_at, finished_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, 'intent', NULL, NULL, NULL, ?)
RETURNING id, task_id, seq, tool_name, class, args_hash, approval_token_id, status, receipt_json, started_at, finished_at, created_at;

-- name: NextToolCallSeq :one
-- The next seq value for a (task_id, tool_name, args_hash) triple (1 for a
-- never-attempted triple).
SELECT COALESCE(MAX(seq), 0) + 1 FROM tool_calls WHERE task_id = ? AND tool_name = ? AND args_hash = ?;

-- name: MarkToolCallExecuting :exec
UPDATE tool_calls
SET status = 'executing', started_at = ?
WHERE id = ?;

-- name: MarkToolCallReceipt :exec
-- The terminal success state: receipt_json (result + result hash) is
-- written in the SAME database transaction that commits the tool's own
-- DB-side effects, immediately once the side effect completes (task spec
-- step 3) - kahyad/internal/task.Receipts.Execute runs this inside the
-- same *sql.Tx as the caller-supplied effect function whenever that
-- effect writes to brain.db itself.
UPDATE tool_calls
SET status = 'receipt', receipt_json = ?, finished_at = ?
WHERE id = ?;

-- name: MarkToolCallFailed :exec
-- Used both when a tool's own effect function returns an error (genuine
-- execution failure) AND by the resume scan, which marks a receipt-less
-- 'intent'/'executing' row 'failed' before deciding whether to auto-retry
-- (W1, within cap) or escalate to blocked_user (W1 past cap, or any
-- W2/W3).
UPDATE tool_calls
SET status = 'failed', finished_at = ?
WHERE id = ?;

-- name: GetReceiptToolCall :one
-- The idempotent-replay lookup (task spec step 4): the most recent
-- status='receipt' row for this exact (task_id, tool_name, args_hash)
-- triple, if any. A hit means "do not re-execute - return this stored
-- receipt_json instead" (kahyad/internal/task.Receipts.Execute), which is
-- the mechanism that makes resume double-execution-safe.
SELECT id, task_id, seq, tool_name, class, args_hash, approval_token_id, status, receipt_json, started_at, finished_at, created_at
FROM tool_calls
WHERE task_id = ? AND tool_name = ? AND args_hash = ? AND status = 'receipt'
ORDER BY seq DESC
LIMIT 1;

-- name: ListReceiptlessToolCalls :many
-- Every tool_calls row for task_id still stuck at 'intent'/'executing' -
-- i.e. a side-effectful call whose kahyad-side execution was interrupted
-- (worker died, kahyad crashed) before a receipt (or a failure) was ever
-- recorded. Ordered most-recent-first: the resume scan (task spec step 6)
-- only ever needs the single most recent one - realistically there is at
-- most one such row per task at any moment, but the scan itself is
-- defensive about that.
SELECT id, task_id, seq, tool_name, class, args_hash, approval_token_id, status, receipt_json, started_at, finished_at, created_at
FROM tool_calls
WHERE task_id = ? AND status IN ('intent', 'executing')
ORDER BY id DESC;

-- name: CountToolCallAttempts :one
-- The per-(task_id, tool_name, args_hash) attempt count the resume scan
-- compares against task.retry.w1_max_auto (task spec step 6: "at most
-- w1_max_auto auto-retries per (task_id, tool_name, args_hash)") - counts
-- every row ever inserted for this exact triple (every seq), regardless
-- of its current status.
SELECT COUNT(*) FROM tool_calls WHERE task_id = ? AND tool_name = ? AND args_hash = ?;

-- name: ListToolCallsByTask :many
-- `kahya task show <id>`'s tool_calls listing, oldest attempt first.
SELECT id, task_id, seq, tool_name, class, args_hash, approval_token_id, status, receipt_json, started_at, finished_at, created_at
FROM tool_calls
WHERE task_id = ?
ORDER BY seq ASC, id ASC;

-- W4-02 outbox lease/redelivery queries. See migrations/
-- 0007_task_durability.sql for the added columns and
-- kahyad/internal/outbox/dispatcher.go for the only caller.

-- name: InsertOutboxRow :one
-- available_at defaults to "now" (immediately dispatchable); lease_until
-- starts NULL (never claimed) and attempts starts 0 - both only ever
-- change via ClaimOutboxRow below.
INSERT INTO outbox (trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts)
VALUES (?, ?, ?, NULL, ?, ?, NULL, 0)
RETURNING id, trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts;

-- name: ListDueOutboxRows :many
-- Candidate rows for one dispatcher claim pass: not yet delivered,
-- available (available_at <= now), and not currently leased by another
-- dispatcher (lease_until IS NULL, i.e. never claimed, OR lease_until has
-- already passed, i.e. a previous claim's lease expired without being
-- acknowledged - the crash-safe re-claim path). Listing candidates and
-- claiming them are two separate steps ON PURPOSE (ClaimOutboxRow below)
-- so two concurrent dispatchers racing on the SAME candidate list each
-- only ever win the atomic UPDATE for rows the other hasn't already
-- claimed first.
SELECT id, trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts
FROM outbox
WHERE dispatched_at IS NULL
  AND available_at <= ?
  AND (lease_until IS NULL OR lease_until < ?)
ORDER BY id ASC
LIMIT ?;

-- name: ClaimOutboxRow :execrows
-- The atomic single-claim guarantee (mirrors ConsumeApprovalToken/
-- ConsumePendingApproval's own "UPDATE ... WHERE <still claimable>"
-- pattern elsewhere in this file): only the FIRST dispatcher to run this
-- UPDATE against a given row - while its lease is still unheld or already
-- expired - ever affects a row; a losing concurrent dispatcher's identical
-- call affects 0 rows and must not touch that row at all. attempts is
-- incremented on every successful claim (task spec step 7: "attempts
-- increments on each claim"), including a crash-safe re-claim after a
-- previous lease expired without the row ever being acknowledged.
UPDATE outbox
SET lease_until = ?, attempts = attempts + 1
WHERE id = ? AND dispatched_at IS NULL AND (lease_until IS NULL OR lease_until < ?);

-- name: MarkOutboxDelivered :exec
-- Terminal success: the re-spawned worker exited 0 (task spec step 7).
UPDATE outbox
SET dispatched_at = ?
WHERE id = ?;

-- name: RenewOutboxLease :exec
-- Task durability BLOCKER 2(b) fix: kahyad/internal/outbox.Dispatcher's
-- heartbeat goroutine calls this every leaseDuration/3 for as long as
-- spawn.Run blocks on a claimed row's re-spawned worker, so a
-- longer-than-one-lease-period task is never re-claimed by a second
-- dispatcher pass purely because its ORIGINAL lease (computed once at
-- claim time) elapsed while the worker was still genuinely running.
-- Scoped to dispatched_at IS NULL defensively - a row this dispatcher has
-- already marked delivered (or that somehow no longer exists) must never
-- have its lease resurrected.
UPDATE outbox
SET lease_until = ?
WHERE id = ? AND dispatched_at IS NULL;

-- W4-03 (taint tiers + Reader/Actor split) queries below. See
-- migrations/0009_session_taint.sql for the schema and
-- kahyad/internal/taint/taint.go for the only caller (a narrow Store
-- interface there - never sqlcgen directly - so kahyad/internal/policy's
-- taint-check hook and kahyad/internal/reader's actor-seeding path both
-- depend on that package's own contract, not on sqlc's generated shapes).

-- name: GetSessionTaint :one
SELECT session_id, tier, reason, updated_at FROM session_taint WHERE session_id = ?;

-- name: InsertSessionTaintClean :exec
-- The ONLY way a 'clean' row is ever created (task spec step 1): a plain
-- INSERT, never an upsert - a session_id that already has ANY row (clean
-- or tainted) makes this fail on the PRIMARY KEY constraint, which
-- kahyad/internal/taint.Tracker.InsertClean treats as a "lowering
-- attempt" (ledgered taint.lower_attempt, rejected) rather than silently
-- overwriting an existing row. reason is always NULL for a clean insert -
-- there is nothing to explain about a session starting clean, unlike
-- RaiseSessionTaint below.
INSERT INTO session_taint (session_id, tier, reason, updated_at)
VALUES (?, 'clean', NULL, ?);

-- name: RaiseSessionTaint :exec
-- The ONLY tier-transition statement in this file: upserts session_id to
-- tier='tainted', creating the row if it did not exist yet (a
-- content-sourced tool output can raise taint on a session before that
-- session's OWN 'clean' row ever landed - the ordering invariant is "no
-- byte reaches the worker before this Raise call", not "the row must
-- already exist") or flipping an existing 'clean' row to 'tainted' (and
-- leaving an already-'tainted' row at 'tainted', updating reason/
-- updated_at regardless - taint only ever rises, so re-raising a
-- different reason is fine, re-raising the SAME tier is a no-op by
-- definition). There is no corresponding statement anywhere in this
-- codebase that ever sets tier back to 'clean' on an existing row.
INSERT INTO session_taint (session_id, tier, reason, updated_at)
VALUES (?, 'tainted', ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
    tier = 'tainted',
    reason = excluded.reason,
    updated_at = excluded.updated_at;

-- name: InsertSessionTaintTainted :exec
-- W5-01's THIRD birth-place for a session_taint row (kahyad/internal/
-- taint's own package doc comment names exactly two before this: the
-- OnSession InsertClean for a user-initiated task, and actor_seed.Spawn's
-- InsertClean for a freshly-seeded Actor). This one is for a session that
-- is UNTRUSTED BY DESIGN AT CREATION (the morning-briefing worker session
-- - HANDOFF S5 safety #2: the briefing is untrusted by design)
-- - never a session that started clean and later had content-sourced
-- taint Raised onto it. A plain INSERT, mirroring InsertSessionTaintClean
-- exactly except for the literal tier: a session_id that already has ANY
-- row makes this fail on the PRIMARY KEY constraint -
-- kahyad/internal/taint.Tracker.InsertUntrusted surfaces that as an error
-- (a caller minting a brand-new session_id that collides with an existing
-- row has a bug worth surfacing), rather than silently reusing
-- RaiseSessionTaint's upsert semantics.
INSERT INTO session_taint (session_id, tier, reason, updated_at)
VALUES (?, 'tainted', ?, ?);

-- W4-05 (ledger external anchor + tamper detection) queries below. See
-- migrations/0010_ledger_anchor.sql for the schema.
--
-- ledger_digest_state queries: kahyad/internal/store.InsertEventWithDigest
-- (the ONE choke point that appends a row to events - see its own doc
-- comment for why it is the only caller of GetLedgerDigestState/
-- AdvanceLedgerDigestState) is the only writer; kahyad/internal/anchor's
-- push.go (to learn what to anchor next) and verify.go (as a bonus
-- cross-check against its own from-genesis recompute) are read-only
-- callers.

-- name: GetLedgerDigestState :one
SELECT id, last_event_id, digest FROM ledger_digest_state WHERE id = 1;

-- name: AdvanceLedgerDigestState :exec
-- Called ONLY from inside InsertEventWithDigest's own transaction,
-- immediately after that same transaction's InsertEvent - both writes
-- commit or roll back together, so the digest can never fall out of sync
-- with the ledger it claims to cover (task spec's own "never an event
-- without its digest step, never a digest step without its event").
UPDATE ledger_digest_state
SET last_event_id = ?, digest = ?
WHERE id = 1;

-- anchor_log queries: kahyad/internal/anchor/push.go is the only writer
-- (InsertAnchorLog/MarkAnchorPushed); push.go and verify.go both read.

-- name: InsertAnchorLog :one
-- status is always 'pending' at insert time (task spec step 3: "insert
-- anchor_log row pending" BEFORE the git push is even attempted) - the
-- caller passes the literal string, never a variable, so this file has
-- exactly one place that ever creates a 'pending' row.
INSERT INTO anchor_log (event_id, digest_hex, anchored_at, remote_ref, status)
VALUES (?, ?, ?, ?, ?)
RETURNING id, event_id, digest_hex, anchored_at, remote_ref, status;

-- name: MarkAnchorPushed :exec
-- The ONLY statement that ever flips a row from 'pending' to 'pushed' -
-- called once the git push has actually landed on the remote (task spec
-- step 3: "On success mark pushed").
UPDATE anchor_log
SET status = 'pushed', remote_ref = ?
WHERE id = ?;

-- name: GetLatestAnchorLog :one
-- The most recent anchor_log row of ANY status - push.go's
-- claimPendingRow uses this to decide whether there is already an
-- in-flight 'pending' row to retry (offline case, task spec step 5) before
-- ever considering a new one.
SELECT id, event_id, digest_hex, anchored_at, remote_ref, status
FROM anchor_log
ORDER BY id DESC
LIMIT 1;

-- name: ListPendingAnchorLogs :many
-- Every not-yet-pushed anchor_log row, oldest first - push.go's
-- stale-pending alarm (task spec step 5: "if the oldest pending is older
-- than 2 x interval_hours, alarm") reads index [0].
SELECT id, event_id, digest_hex, anchored_at, remote_ref, status
FROM anchor_log
WHERE status = 'pending'
ORDER BY id ASC;

-- name: ListAnchorLogs :many
-- Every anchor_log row ordered by the ledger position it anchors -
-- verify.go's own recompute-from-event-1 pass uses this as its ordered
-- checkpoint list (task spec step 6: "at each anchor_log/remote anchor
-- line, compare").
SELECT id, event_id, digest_hex, anchored_at, remote_ref, status
FROM anchor_log
ORDER BY event_id ASC;

-- name: ListAllEvents :many
-- Every ledger event ever appended, oldest first - verify.go's own
-- full-recompute pass (task spec step 6: "recompute the digest from event
-- 1 upward"; "Full recompute is fine at MVP scale ... do not optimize").
SELECT id, trace_id, ts, kind, payload, created_at
FROM events
ORDER BY id ASC;

-- name: CountEventsByKindAndDate :one
-- W5-01's once-per-day idempotency check: counts events of kind on the
-- UTC calendar date dateStr ("YYYY-MM-DD" - SQLite's date() truncates
-- created_at to exactly this, and every created_at this codebase writes
-- is already UTC RFC3339/RFC3339Nano, so no timezone conversion is
-- needed). kahyad/internal/briefing.Orchestrator.Run consults this
-- BEFORE ever classifying a single collector item or spawning a worker
-- for a scheduled OR manual run: a non-zero count for kind=
-- "briefing.delivered" means today's briefing already went out, so this
-- run logs briefing.skipped_duplicate and sends nothing - a missed-run-
-- fired-on-wake plus the regular 08:30 run can therefore never deliver
-- two notifications the same date.
SELECT count(*) FROM events WHERE kind = ? AND date(created_at) = ?;
