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
-- ALTER TABLEs, and worker_pgid/halted_at, W6-03's 0015_halt_semantics
-- ALTER TABLEs - all left OUT of the INSERT column/VALUES list itself, so
-- every existing caller keeps inserting a row that takes their DEFAULTs
-- unchanged: status='intent', attempts=0, next_retry_at=NULL,
-- worker_pgid=NULL, halted_at=NULL) so sqlc reuses the existing Task
-- model type here rather than generating a second, differently-ordered
-- InsertTaskRow type.
INSERT INTO tasks (id, trace_id, session_id, state, model, envelope, updated_at, created_at, lane, secret_category)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, trace_id, session_id, state, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts, worker_pgid, halted_at;

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
SELECT id, trace_id, session_id, state, model, envelope, updated_at, created_at
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
SELECT token_hash, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at, revoked_at
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

-- name: ListUnconsumedPendingApprovalsByTask :many
-- W6-03 Engine.InvalidateApprovalsForTask's own candidate set: every
-- not-yet-consumed pending_approvals row for taskID, oldest first - the
-- SAME "not-yet-consumed" definition ListUnconsumedPendingApprovals above
-- uses, scoped to one task instead of every task in the system.
SELECT id, task_id, trace_id, tool, class, scope, approved_bytes_hash, minted_at, expires_at, consumed_at, tool_input
FROM pending_approvals
WHERE task_id = ? AND consumed_at IS NULL
ORDER BY minted_at ASC;

-- name: RevokeApprovalTokensByTask :execrows
-- W6-03 halt executor step 3.5 ("invalidate pending approvals ... and
-- REVOKE their one-time tokens"): burns every not-yet-consumed
-- approval_tokens row for taskID via the SAME "UPDATE ... WHERE
-- consumed_at IS NULL" single-use pattern ConsumeApprovalToken above uses
-- - a token an autonomy-ladder auto-allow (W1) or a prior human Approve
-- (W2) already minted BEFORE the halt, but that the corresponding
-- side-effectful MCP tool has not yet presented to ConsumeToken, is
-- exactly the "a diff approved before the halt must not authorize
-- anything after it" case (HANDOFF S5 one-time approval tokens) - a stale
-- CLI decide, Hammerspoon card button, or Telegram inline button pressed
-- AFTER this UPDATE all hit ConsumeToken's identical "0 rows affected ->
-- ErrTokenInvalid" fail-closed path, never a demotion (this is the user
-- choosing to stop, not a tool misusing a token - see
-- kahyad/internal/halt's own doc comment for why this bypasses
-- Engine.ConsumeToken's demotion machinery entirely).
UPDATE approval_tokens
SET consumed_at = ?, revoked_at = ?
WHERE task_id = ? AND consumed_at IS NULL;

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
SELECT id, trace_id, session_id, state, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts, worker_pgid, halted_at
FROM tasks
WHERE id = ?;

-- name: SetTaskWorkerPGID :exec
-- W6-03: persists the spawned worker's process-group id onto its task row
-- (kahyad/internal/spawn.Callbacks.OnStart fires this at BOTH the first
-- spawn, kahyad/internal/server's handleTask, AND every outbox-driven
-- redispatch, kahyad/internal/outbox.Dispatcher.processResume) -
-- ALONGSIDE the existing in-memory kahyad/internal/task.LiveRegistry, so
-- the W6-03 halt executor can still find and kill this worker's process
-- GROUP even after a daemon crash/restart emptied that in-memory registry
-- (macOS has no PDEATHSIG - see migrations/0015_halt_semantics.sql's own
-- doc comment).
UPDATE tasks
SET worker_pgid = ?, updated_at = ?
WHERE id = ?;

-- name: SetTaskHaltedAt :exec
-- W6-03: stamped by the halt executor in the SAME step that transitions
-- tasks.status to 'user_halted' (kahyad/internal/task.Machine.Transition,
-- a separate call - this query ONLY ever touches halted_at, exactly like
-- SetTaskNextRetry above only ever touches next_retry_at).
UPDATE tasks
SET halted_at = ?, updated_at = ?
WHERE id = ?;

-- name: ListNonTerminalTasks :many
-- W6-03 `POST /halt {"all":true}`'s own candidate set (task spec step 4:
-- "{all:true} iterates every task in a non-terminal running state"): every
-- task NOT already in one of the three terminal statuses (done, failed,
-- user_halted - kahyad/internal/task.allowedTransitions' own zero-outbound-
-- edges states). Halting an already-terminal task is instead the
-- idempotent no-op HaltTask itself decides on a direct GetTaskByID lookup
-- (task spec step 8) - this query is only ever consulted for the {all:true}
-- fan-out, never for a single --task <id> halt.
SELECT id, trace_id, session_id, state, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts, worker_pgid, halted_at
FROM tasks
WHERE status NOT IN ('done', 'failed', 'user_halted')
ORDER BY updated_at ASC;

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
--
-- W6-03 defense-in-depth (HANDOFF S6 W6 flag, verbatim): "gorev terminal
-- user_halted durumuna yazilir (session-resume ve outbox retry'dan kalici
-- haric)". status = 'executing' above ALREADY excludes 'user_halted' (the
-- two are mutually exclusive column values - a halted task can never also
-- read 'executing'), so the explicit "AND status != 'user_halted'" below
-- is redundant by construction today; it is kept anyway, verbatim, as a
-- second, independent guard directly at this query - the W6-03 task spec's
-- own instruction ("defense in depth even though it is terminal") for the
-- exact scenario this comment quotes: even a future change to this WHERE
-- clause that loosens 'executing' can never accidentally resume a halted
-- task without ALSO having to delete this line first.
SELECT id, trace_id, session_id, state, model, envelope, updated_at, created_at, lane, secret_category, status, next_retry_at, attempts, worker_pgid, halted_at
FROM tasks
WHERE status = 'executing'
  AND status != 'user_halted'
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
-- change via ClaimOutboxRow below. task_id (W6-03, migrations/
-- 0015_halt_semantics.sql) back-links this row to the task it would
-- redeliver - NULL for any future outbox kind that is not task-scoped
-- (none exist yet; kahyad/internal/task.writeOutboxResumeRowAt is the only
-- caller today and always supplies one). canceled_at starts NULL, exactly
-- like dispatched_at - only the W6-03 halt executor's CancelOutboxRowsByTask
-- ever sets it.
INSERT INTO outbox (trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts, task_id, canceled_at)
VALUES (?, ?, ?, NULL, ?, ?, NULL, 0, ?, NULL)
RETURNING id, trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts, task_id, canceled_at;

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
--
-- W6-03 (HANDOFF S6 W6 flag, verbatim): "gorev terminal user_halted
-- durumuna yazilir (session-resume ve outbox retry'dan kalici haric)".
-- TWO independent, deliberately redundant guards enforce this at the
-- claim query itself (task spec step 5: "must filter status !=
-- 'user_halted' explicitly, even though it is terminal"):
--   1. o.canceled_at IS NULL - the halt executor's own
--      CancelOutboxRowsByTask marks every one of the task's undelivered
--      rows canceled AT HALT TIME (task spec step 3.4).
--   2. the LEFT JOIN below re-checks the OWNING TASK's CURRENT status on
--      every claim pass, straight from tasks.status - so even a row that
--      somehow went uncancelled (a bug, a race, a future caller of
--      InsertOutboxRow that forgets task_id) is STILL never claimed for a
--      task that is (or has since become) user_halted. t.id IS NULL
--      covers a row whose task_id is NULL (no task to exclude it - see
--      InsertOutboxRow's own doc comment) exactly as before this guard
--      existed.
SELECT o.id, o.trace_id, o.kind, o.payload, o.dispatched_at, o.created_at, o.available_at, o.lease_until, o.attempts, o.task_id, o.canceled_at
FROM outbox o
LEFT JOIN tasks t ON t.id = o.task_id
WHERE o.dispatched_at IS NULL
  AND o.canceled_at IS NULL
  AND o.available_at <= ?
  AND (o.lease_until IS NULL OR o.lease_until < ?)
  AND (t.id IS NULL OR t.status != 'user_halted')
ORDER BY o.id ASC
LIMIT ?;

-- name: CancelOutboxRowsByTask :execrows
-- W6-03 halt executor step 3.4: cancel every UNDELIVERED outbox row for
-- taskID (dispatched_at IS NULL - a row already marked delivered is left
-- untouched, matching this codebase's "never rewrite settled history"
-- posture elsewhere). Idempotent (AND canceled_at IS NULL): a second halt
-- call against an already-halted task affects 0 rows here, never
-- re-stamps canceled_at with a later timestamp.
UPDATE outbox
SET canceled_at = ?
WHERE task_id = ? AND dispatched_at IS NULL AND canceled_at IS NULL;

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

-- W5-02 (nightly consolidation) queries below: hot-window detail-atom
-- promotion (facts INSERT, episodes.cooled_at) - see
-- kahyad/internal/consolidation/hotwindow.go.

-- name: ListChunksByEpisode :many
-- Raw chunk text for one episode, in sequence order - hotwindow.go scans
-- this text for detail atoms (numbers/dates/quotes/decisions/promises).
-- Every fact promoted from it cites these chunk ids as evidence, never a
-- prior summary (HANDOFF S5 memory #4: "her ozet ham kanittan uretilir,
-- asla alt-ozetten").
SELECT id, episode_id, seq, text, content_hash, created_at
FROM chunks
WHERE episode_id = ?
ORDER BY seq ASC;

-- name: ListUncooledEpisodesOlderThan :many
-- Active episodes not yet hot-window-cooled, created at or before cutoff
-- (RFC3339 UTC string compare - every created_at this codebase writes is
-- already normalized to that format, so a plain string comparison sorts
-- correctly, matching CountEventsByKindAndDate's own date()-free
-- convention above for the same reason).
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at
FROM episodes
WHERE status = 'active' AND cooled_at IS NULL AND created_at <= ?
ORDER BY id ASC;

-- name: MarkEpisodeCooled :exec
-- Stamped ONLY after this episode's detail atoms have been promoted to
-- facts (task spec step 6: "only then mark cooled").
UPDATE episodes SET cooled_at = ? WHERE id = ?;

-- name: InsertFact :one
-- One hot-window candidate fact: source_tier is ALWAYS 'agent_derived'
-- here (quarantined from profile-card/injection until a human confirms -
-- kahyad/internal/server's own quarantinedSourceTier), evidentiality is
-- 'inferred' (the extractor read the raw chunk text, it did not witness
-- or get told the fact directly), confidence is a caller-supplied
-- log-odds value (this column is LOG-ODDS, never a bare probability -
-- 0001_init_schema.sql's own header note), evidence cites the raw
-- episode/chunk this fact was promoted from (e.g. "episode:12,chunk:34"),
-- never a prior summary.
INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, importance, valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, subject, predicate, object, source_tier, evidentiality, confidence, importance, valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at, confirmed_at;

-- W5-04 (memory-correctness-engine) queries below: the single fact-write
-- path (kahyad/internal/factengine) is the ONLY caller of InsertFact
-- above and everything in this section - see that package's own doc
-- comment for the source-trust lattice / log-odds / entity-merge rules
-- these back.

-- name: GetFact :one
-- Looked up by kahya fact confirm/retract (CLI, over UDS) and by the
-- engine's own recompute-from-evidence path.
SELECT id, subject, predicate, object, source_tier, evidentiality, confidence, importance, valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at, confirmed_at
FROM facts
WHERE id = ?;

-- name: GetActiveFactByTriple :one
-- WriteFact's upsert lookup: an existing ACTIVE fact for this exact
-- (subject, predicate, object) gets a new evidence row instead of a
-- duplicate fact row (HANDOFF S5 memory #3: repeated assertions
-- accumulate evidence on ONE fact, they never mint a second one). A
-- retracted/closed fact is deliberately excluded (status='active' only)
-- so a later re-assertion after a retraction creates a FRESH fact rather
-- than reviving the closed one in place - the retracted row stays exactly
-- as HANDOFF S5 memory #3 requires (closed, never deleted, never
-- reopened).
SELECT id, subject, predicate, object, source_tier, evidentiality, confidence, importance, valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at, confirmed_at
FROM facts
WHERE subject = ? AND predicate = ? AND object = ? AND status = 'active'
ORDER BY id DESC
LIMIT 1;

-- name: UpdateFactConfidence :exec
-- The engine recomputes confidence from the fact's own evidence rows
-- (sum of weights, deduped by (session_id, polarity), clamped to the
-- highest positive tier cap represented - never a noisy-OR ratchet) and
-- writes the result back here after every WriteFact/DenyFact call.
UPDATE facts SET confidence = ?, updated_at = ? WHERE id = ?;

-- name: ConfirmFact :exec
-- `kahya fact confirm <id>` (or a W5-03 ritual Dogru answer): lifts the
-- agent_derived quarantine half of the injection-eligibility predicate.
-- Never touches source_tier or confidence - an agent_derived fact stays
-- agent_derived, capped at that tier's ceiling, forever (HANDOFF S5
-- memory #1).
UPDATE facts SET confirmed_at = ?, updated_at = ? WHERE id = ?;

-- name: RetractFact :exec
-- Closes a fact (HANDOFF S5 memory #3: "Artik sevmiyorum" -> geri-cekme):
-- valid_to set, status='retracted' - NEVER a DELETE. The caller
-- (kahyad/internal/factengine/retract.go) always inserts a negative
-- evidence row in the SAME logical operation, never this alone.
UPDATE facts SET status = 'retracted', valid_to = ?, updated_at = ? WHERE id = ?;

-- name: ListEvidenceByFact :many
-- Every evidence row for one fact, in insertion order - the engine's
-- confidence-recompute path sums .weight over these (deduped by
-- (session_id, polarity) defensively, though WriteFact/DenyFact already
-- refuse to insert a second row for a (fact_id, session_id, polarity)
-- already on file - HANDOFF S5 memory #3's "ayni-oturum tekrari tek kanit
-- sayilir").
SELECT id, fact_id, episode_id, session_id, polarity, weight, created_at
FROM evidence
WHERE fact_id = ?
ORDER BY id ASC;

-- name: GetEvidenceByFactSessionPolarity :one
-- The per-(fact_id, session_id, polarity) dedupe check (HANDOFF S5
-- memory #3): sql.ErrNoRows means this session has not yet evidenced
-- this fact at this polarity, so a NEW evidence row is due; a hit means
-- the existing row already covers it and no second row is ever inserted
-- (a repeat within the SAME session is not a second, independent
-- observation).
SELECT id, fact_id, episode_id, session_id, polarity, weight, created_at
FROM evidence
WHERE fact_id = ? AND session_id = ? AND polarity = ?;

-- name: InsertEvidence :one
-- weight is the signed log-odds delta this ONE row contributes (positive
-- for a supporting tier's fixed constant, the fixed user-denial constant
-- for a negative/retraction row) - see this file's own migrations/
-- 0012_factengine.sql comment on evidence.weight for why this column
-- exists at all.
INSERT INTO evidence (fact_id, episode_id, session_id, polarity, weight, created_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, fact_id, episode_id, session_id, polarity, weight, created_at;

-- name: InsertEntity :one
-- provisional=1 marks a suspicious same-name entity (HANDOFF S5 memory
-- #2: "supheli ayni-isim -> yeni gecici varlik") - kahyad/internal/
-- factengine/entity.go sets this whenever canonical_name already
-- collides with an EXISTING entity's alias; 0 for the first entity ever
-- seen under a given name.
INSERT INTO entities (canonical_name, kind, status, provisional, created_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, canonical_name, kind, status, provisional, created_at;

-- name: GetEntity :one
SELECT id, canonical_name, kind, status, provisional, created_at
FROM entities
WHERE id = ?;

-- name: UpdateEntityStatus :exec
-- Merge marks the LOSING entity 'merged' (never deleted - merge_ledger
-- plus this status flip is the whole audit trail); split flips it back
-- to 'active'.
UPDATE entities SET status = ? WHERE id = ?;

-- name: ListEntityIDsByAlias :many
-- Every DISTINCT existing entity already registered under alias - empty
-- means "no collision, safe to create the first entity for this name";
-- one or more existing ids means a NEW entity must be created provisional
-- (HANDOFF S5 memory #2: name similarity alone never auto-merges).
SELECT DISTINCT entity_id FROM entity_aliases WHERE alias = ?;

-- name: InsertEntityAlias :one
INSERT INTO entity_aliases (entity_id, alias, created_at)
VALUES (?, ?, ?)
RETURNING id, entity_id, alias, created_at;

-- name: ListEntityAliasesByEntity :many
-- Snapshotted into merge_ledger.evidence BEFORE a merge reassigns them,
-- so Split can restore exactly this set back onto the losing entity.
SELECT id, entity_id, alias, created_at
FROM entity_aliases
WHERE entity_id = ?
ORDER BY id ASC;

-- name: UpdateEntityAliasEntityByID :exec
-- Moves ONE alias row (by its own id, never by entity_id bulk-match) onto
-- a different entity - Merge moves every alias row it snapshotted off the
-- losing entity onto the surviving one; Split moves that EXACT same set
-- of row ids back, which a bulk "WHERE entity_id = ?" reassignment could
-- not do once the surviving entity ALSO owns its own pre-existing aliases
-- (a bulk move-back would wrongly sweep those up too).
UPDATE entity_aliases SET entity_id = ? WHERE id = ?;

-- name: InsertMergeLedger :one
-- One row per merge AND per split (HANDOFF S5 memory #2: "Merge-defteri +
-- varlik-bolme operasyonu") - never updated, never deleted, matching the
-- append-only posture the events ledger enforces at the SQL level (this
-- table has no such trigger, but kahyad/internal/factengine never issues
-- an UPDATE/DELETE against it either).
INSERT INTO merge_ledger (op, src_entity_id, dst_entity_id, evidence, actor, created_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, op, src_entity_id, dst_entity_id, evidence, actor, created_at;

-- name: GetMergeLedger :one
SELECT id, op, src_entity_id, dst_entity_id, evidence, actor, created_at
FROM merge_ledger
WHERE id = ?;

-- W5-03 (truth-ritual) queries below: the eval_labels durable label store
-- plus the two small facts/episodes reads kahyad/internal/ritual's sampler
-- needs beyond what factengine/W5-04 already added above. See that
-- package for the only callers.

-- name: ListActiveFacts :many
-- The ritual sampler's whole candidate pool - every ACTIVE fact,
-- unfiltered (kahyad/internal/ritual/select.go applies the secret-lane
-- exclusion/priority policy entirely in Go, never in SQL, so the policy
-- stays readable/testable as plain code, not buried in a query).
SELECT id, subject, predicate, object, source_tier, evidentiality, confidence, importance, valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at, confirmed_at
FROM facts
WHERE status = 'active'
ORDER BY id ASC;

-- name: GetEpisodeByID :one
-- The sampler's fail-closed secret-lane classification read: resolves one
-- evidence row's episode_id to its source_path, matched against
-- policy.yaml's secret_lane_globs - the SAME path-glob mechanism
-- kahyad/internal/consolidation's PartitionByLane already uses for memory
-- files (HANDOFF S4 ordering invariant: "policy.yaml globlari YALNIZ
-- dosya yollari icin").
SELECT id, source, source_path, source_hash, source_tier, started_at, ended_at, status, meta, created_at, cooled_at
FROM episodes
WHERE id = ?;

-- name: InsertEvalLabel :one
-- One row per fact ASKED about in a single ritual run (W5-03 task spec
-- step 4) - label/answered_at start NULL, filled in by
-- UpdateEvalLabelAnswer once (and if) a Telegram callback arrives inside
-- the 72h expiry window kahyad/internal/ritual.Engine.Answer enforces.
INSERT INTO eval_labels (fact_id, question_text, label, asked_at, answered_at, channel, trace_id, created_at)
VALUES (?, ?, NULL, ?, NULL, ?, ?, ?)
RETURNING id, fact_id, question_text, label, asked_at, answered_at, channel, trace_id, created_at;

-- name: GetEvalLabel :one
SELECT id, fact_id, question_text, label, asked_at, answered_at, channel, trace_id, created_at
FROM eval_labels
WHERE id = ?;

-- name: UpdateEvalLabelAnswer :exec
-- Edits label/answered_at IN PLACE (W5-03 task spec: "multiple taps on
-- the same question edit the label, they do not append evidence rows") -
-- called again on a later, possibly-different-button tap for the SAME
-- question; this never creates a second eval_labels row for one asked
-- fact.
UPDATE eval_labels
SET label = ?, answered_at = ?
WHERE id = ?;

-- name: ListEvalLabelsByTrace :many
-- Every eval_labels row one ritual run minted, oldest first - the "all
-- rows share one trace_id" acceptance criterion's own read path.
SELECT id, fact_id, question_text, label, asked_at, answered_at, channel, trace_id, created_at
FROM eval_labels
WHERE trace_id = ?
ORDER BY id ASC;

-- name: CountUnansweredEvalLabelsByTrace :one
SELECT count(*) FROM eval_labels WHERE trace_id = ? AND answered_at IS NULL;

-- name: ListLastAskedAtAllFacts :many
-- Per-fact most-recent asked_at, across every ritual run ever run - the
-- sampler's "never/longest-ago probed" priority tier (select.go): a
-- fact_id absent from this result set has never been asked and sorts
-- first, ahead of every fact that has.
SELECT fact_id, MAX(asked_at) AS last_asked_at
FROM eval_labels
GROUP BY fact_id;

-- name: ListRitualLabeledFactsForEval :many
-- W78-01 `kahya eval export-ritual`: every ANSWERED (label IS NOT NULL)
-- ritual label joined to its fact, newest label first - the draft source
-- the CLI prints to stdout for MANUAL curation into the private ~/Kahya
-- retrieval dataset. Read-only; adds no columns, so no migration is needed.
SELECT el.fact_id, el.label, el.question_text, f.subject, f.predicate, f.object
FROM eval_labels el
JOIN facts f ON f.id = el.fact_id
WHERE el.label IS NOT NULL
ORDER BY el.answered_at DESC, el.id DESC;
