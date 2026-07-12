// resume.go implements the W4-02 crash-recovery resume scan (task spec
// step 6): find every tasks.status='executing' row with no LIVE worker
// process (kahyad startup, and a periodic tick thereafter), and decide,
// per task, whether it is safe to simply re-dispatch the worker (nothing
// was interrupted mid-side-effect, or the interrupted call already has a
// durable receipt - see receipts.go's idempotent replay) or whether a
// human must decide (a receipt-less W1 call past its auto-retry cap, or
// any receipt-less W2/W3 call at all - HANDOFF §6 W4 ⚑: "makbuzsuz
// executing'de yalniz W1 oto-tekrar, W2/W3 asla").
//
// The actual worker re-spawn is kahyad/internal/outbox.Dispatcher's job
// (a sibling package): this file only ever WRITES a row into the shared
// outbox table (enqueueResume) - it never imports kahyad/internal/outbox
// itself, so there is no import cycle (outbox depends on task, not the
// other way around).
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// fixedNanoRFC3339Layout formats a time with a FIXED 9-digit fractional
// second component that is NEVER trimmed - unlike time.RFC3339Nano
// (whose trailing-zero-trimmed fractional seconds do NOT always sort
// lexicographically in timestamp order: e.g. "10:00:00Z" - a value that
// landed exactly on a zero-nanosecond boundary, printed with NO decimal
// point at all - sorts LEXICOGRAPHICALLY AFTER "10:00:00.5Z", a LATER
// instant in the same second, because '.' < 'Z' as bytes; see
// kahyad/internal/store/queries/queries.sql's ListUnconsumedPendingApprovals
// comment for this exact, already-documented pitfall elsewhere in this
// codebase - that is why every other plain-TEXT timestamp comparison in
// this codebase happens in GO, never in SQL). outbox.available_at/
// lease_until are the one place this task's own dispatcher NEEDS a
// correct SQL-side inequality (ClaimOutboxRow's atomic compare-and-swap
// claim, and ListDueOutboxRows' own due-row filter) - FixedNanoRFC3339
// below is what makes that safe: a FIXED-width format sorts
// lexicographically identically to chronological order, with no trimming
// pitfall possible.
const fixedNanoRFC3339Layout = "2006-01-02T15:04:05.000000000Z07:00"

// FixedNanoRFC3339 formats t (converted to UTC) using
// fixedNanoRFC3339Layout - see that constant's own doc comment. Used for
// EVERY outbox.available_at/lease_until value this package or
// kahyad/internal/outbox ever writes, so plain SQL TEXT `<=`/`<`
// comparisons against those two columns are always correct.
func FixedNanoRFC3339(t time.Time) string {
	return t.UTC().Format(fixedNanoRFC3339Layout)
}

// OutboxKindTaskResume is the outbox.kind value every row enqueueResume
// writes - kahyad/internal/outbox.Dispatcher's claim loop dispatches on
// this exact string.
const OutboxKindTaskResume = "task_resume"

// OutboxTaskResumePayload is OutboxKindTaskResume's outbox.payload JSON
// shape - exported so kahyad/internal/outbox can decode a claimed row
// without this package needing to expose anything else about how a
// resume row is represented.
type OutboxTaskResumePayload struct {
	TaskID string `json:"task_id"`
}

// Turkish user-facing notification strings (task spec step 6, byte-exact
// - CLAUDE.md language policy). %s/%s/%d/%s substitute task_id, tool
// name, attempt count, task_id again (the second "kahya task resolve
// <id>" mention).
const (
	// MsgW1RetryCapExceededFmt is used when a receipt-less W1 tool call
	// has now been interrupted more times than task.retry.w1_max_auto
	// allows.
	MsgW1RetryCapExceededFmt = "Görev %s: '%s' aracı %d kez yarıda kesildi (W1). Otomatik tekrar limiti doldu — 'kahya task resolve %s' ile karar ver."
	// MsgW2W3ReceiptlessFmt is used for ANY receipt-less W2/W3 tool call -
	// there is no auto-retry cap to hit, because there is no auto-retry at
	// all (HANDOFF §6 W4 ⚑).
	MsgW2W3ReceiptlessFmt = "Görev %s: '%s' aracı yarıda kesildi ve makbuzu yok. W2/W3 sınıfı olduğu için otomatik tekrarlanmadı — 'kahya task resolve %s' ile karar ver."
)

// Notification event kinds this file's blocked_user path ledgers via
// Notifier.Notify (which itself ledgers a `kind` row carrying the
// message - see kahyad/internal/notify.JSONLNotifier.Notify).
const (
	EventW1RetryCapExceeded = "task.w1_retry_cap_exceeded"
	EventW2W3Receiptless    = "task.w2w3_receiptless_blocked"
)

// defaultW1MaxAuto mirrors config.Config's own default (kept as a small,
// local literal - this package does not import kahyad/internal/config,
// to avoid a dependency an internal state-machine package has no other
// reason to take; NewResume's caller (main.go) always threads the real
// configured value through).
const defaultW1MaxAuto = 3

// Notifier is the narrow notification surface Resume needs.
// *notify.JSONLNotifier already has exactly this method shape (a subset
// of kahyad/internal/notify.Notifier), so it satisfies this with no
// adapter code.
type Notifier interface {
	Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}

// LiveChecker reports whether a task currently has a live, kahyad-owned
// worker process - see LiveRegistry (live.go) for the concrete
// implementation main.go wires in. A nil LiveChecker (Resume's own
// zero-dependency posture) means every 'executing' task is treated as
// NOT live, which is exactly correct for the kahyad-startup scan (nothing
// can possibly still be running - the whole process, and everything it
// had spawned, just (re)started).
type LiveChecker interface {
	IsLive(taskID string) bool
}

// OutboxEnqueuer is the narrow outbox-table write surface enqueueResume
// needs. *sqlcgen.Queries satisfies this directly.
type OutboxEnqueuer interface {
	InsertOutboxRow(ctx context.Context, arg sqlcgen.InsertOutboxRowParams) (sqlcgen.Outbox, error)
}

var _ OutboxEnqueuer = (*sqlcgen.Queries)(nil)

// Resume implements the W4-02 crash-recovery resume scan.
type Resume struct {
	store     Store
	toolCalls ToolCallStore
	outbox    OutboxEnqueuer
	machine   *Machine
	notifier  Notifier
	live      LiveChecker
	w1MaxAuto int
	now       func() time.Time
}

// NewResume constructs a Resume. live may be nil (every 'executing' task
// is then treated as not-live - correct for a kahyad-startup-only caller;
// a caller that ALSO scans periodically WHILE the daemon is alive must
// pass a real LiveChecker (LiveRegistry) or every in-flight task the
// daemon itself is still running would be wrongly treated as crashed).
// w1MaxAuto <= 0 is normalized to defaultW1MaxAuto (config.Config.
// TaskRetryW1MaxAuto's own default, kept in sync by hand).
func NewResume(store Store, toolCalls ToolCallStore, outbox OutboxEnqueuer, machine *Machine, notifier Notifier, live LiveChecker, w1MaxAuto int) *Resume {
	if w1MaxAuto <= 0 {
		w1MaxAuto = defaultW1MaxAuto
	}
	return &Resume{
		store: store, toolCalls: toolCalls, outbox: outbox, machine: machine,
		notifier: notifier, live: live, w1MaxAuto: w1MaxAuto, now: time.Now,
	}
}

// SetClock overrides Resume's clock (tests only).
func (r *Resume) SetClock(now func() time.Time) { r.now = now }

func (r *Resume) nowRFC3339() string { return r.now().UTC().Format(time.RFC3339Nano) }

// Scan finds every tasks.status='executing' row with no live worker
// (LiveChecker) and runs ProcessTask against each, returning how many it
// examined. A single task's processing error is logged into the return
// error's Join-free "keep going" posture: Scan does not abort the whole
// scan just because one task's bookkeeping failed - it returns the FIRST
// error encountered (if any) after having still attempted every
// candidate, so a caller can log it without the rest of the scan being
// silently skipped.
func (r *Resume) Scan(ctx context.Context) (int, error) {
	tasks, err := r.store.ListExecutingTasks(ctx)
	if err != nil {
		return 0, fmt.Errorf("task: resume scan list executing tasks: %w", err)
	}

	var firstErr error
	n := 0
	for _, t := range tasks {
		if r.live != nil && r.live.IsLive(t.ID) {
			continue // the daemon itself is still actively running this task
		}
		n++
		if err := r.ProcessTask(ctx, t); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("task: resume scan process %s: %w", t.ID, err)
		}
	}
	return n, firstErr
}

// ProcessTask implements task spec step 6's decision tree for a single
// not-live, still-'executing' task:
//
//   - No receipt-less tool_calls row at all -> nothing was interrupted
//     mid-side-effect (or the interrupted call already has a durable
//     receipt, from Receipts.Execute's own idempotent-replay guarantee) ->
//     requeue for resume directly.
//   - The most recent receipt-less row is class W1 -> mark it failed,
//     count every attempt ever made for this exact (task_id, tool_name,
//     args_hash) triple; within task.retry.w1_max_auto -> requeue for
//     resume (auto-retry); past the cap -> blocked_user + the exact
//     Turkish W1-cap string.
//   - The most recent receipt-less row is class W2/W3 -> mark it failed,
//     blocked_user + the exact Turkish W2/W3 string. No auto-retry at all.
func (r *Resume) ProcessTask(ctx context.Context, t sqlcgen.Task) error {
	traceID := t.TraceID

	rows, err := r.toolCalls.ListReceiptlessToolCalls(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list receipt-less tool_calls: %w", err)
	}
	if len(rows) == 0 {
		return r.enqueueResume(ctx, traceID, t.ID)
	}

	row := rows[0]
	if err := r.toolCalls.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{
		FinishedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: row.ID,
	}); err != nil {
		return fmt.Errorf("mark tool_calls failed (id=%d): %w", row.ID, err)
	}

	if row.Class != string(ClassW1) {
		return r.blockOnReceiptless(ctx, traceID, t.ID, row.ToolName)
	}

	attempts, err := r.toolCalls.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{
		TaskID: t.ID, ToolName: row.ToolName, ArgsHash: row.ArgsHash,
	})
	if err != nil {
		return fmt.Errorf("count tool_calls attempts: %w", err)
	}
	if int(attempts) > r.w1MaxAuto {
		return r.blockOnW1CapExceeded(ctx, traceID, t.ID, row.ToolName, attempts)
	}

	return r.enqueueResume(ctx, traceID, t.ID)
}

// blockOnW1CapExceeded implements the W1-past-cap branch: transition the
// task to blocked_user and notify with the exact Turkish string.
func (r *Resume) blockOnW1CapExceeded(ctx context.Context, traceID, taskID, tool string, attempts int64) error {
	if err := r.machine.Transition(ctx, traceID, taskID, StatusBlockedUser); err != nil {
		return fmt.Errorf("transition to blocked_user: %w", err)
	}
	msg := fmt.Sprintf(MsgW1RetryCapExceededFmt, taskID, tool, attempts, taskID)
	return r.notify(ctx, traceID, EventW1RetryCapExceeded, msg, taskID, tool, attempts)
}

// blockOnReceiptless implements the W2/W3-receipt-less branch: transition
// the task to blocked_user and notify with the exact Turkish string.
// There is no attempt count in this message - there is no cap, because
// there is no auto-retry at all for W2/W3 (HANDOFF §6 W4 ⚑).
func (r *Resume) blockOnReceiptless(ctx context.Context, traceID, taskID, tool string) error {
	if err := r.machine.Transition(ctx, traceID, taskID, StatusBlockedUser); err != nil {
		return fmt.Errorf("transition to blocked_user: %w", err)
	}
	msg := fmt.Sprintf(MsgW2W3ReceiptlessFmt, taskID, tool, taskID)
	return r.notify(ctx, traceID, EventW2W3Receiptless, msg, taskID, tool, 0)
}

func (r *Resume) notify(ctx context.Context, traceID, kind, msg, taskID, tool string, attempts int64) error {
	if r.notifier == nil {
		return nil
	}
	payload := map[string]any{"task_id": taskID, "tool": tool}
	if attempts > 0 {
		payload["attempts"] = attempts
	}
	return r.notifier.Notify(ctx, traceID, kind, msg, payload)
}

// writeOutboxResumeRow writes one OutboxKindTaskResume row available
// IMMEDIATELY (availableAt=now) that kahyad/internal/outbox.Dispatcher
// will claim and act on by re-spawning the worker with the stored
// session_id + resume:true (task spec step 7/8). It does NOT touch
// tasks.attempts - callers that are redispatching a task WITHOUT any
// status transition (this file's own enqueueResume, below) must bump
// attempts themselves before calling this; a caller reaching here via a
// status transition INTO 'executing' (resolve.go's Resolver.Retry) gets
// its attempts bump for free from Machine.Transition itself and must not
// double-count it by bumping again here.
func writeOutboxResumeRow(ctx context.Context, outbox OutboxEnqueuer, now func() time.Time, traceID, taskID string) error {
	ts := FixedNanoRFC3339(now())
	return writeOutboxResumeRowAt(ctx, outbox, traceID, taskID, ts, ts)
}

// writeOutboxResumeRowAt is writeOutboxResumeRow generalized to a caller-
// chosen (createdAt, availableAt) pair - cloudretry.go's park() is the
// one caller that needs availableAt in the FUTURE (next_retry_at, task
// spec step 3): the SAME OutboxKindTaskResume row shape and the SAME
// kahyad/internal/outbox.Dispatcher claim loop handle it with no new
// mechanism, simply picking it up once available_at has passed (exactly
// the task spec's own "no new mechanism here" instruction).
func writeOutboxResumeRowAt(ctx context.Context, outbox OutboxEnqueuer, traceID, taskID, createdAt, availableAt string) error {
	payload, err := json.Marshal(OutboxTaskResumePayload{TaskID: taskID})
	if err != nil {
		return fmt.Errorf("marshal resume payload: %w", err)
	}
	if _, err := outbox.InsertOutboxRow(ctx, sqlcgen.InsertOutboxRowParams{
		TraceID: traceID, Kind: OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: createdAt, AvailableAt: sql.NullString{String: availableAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("enqueue outbox row: %w", err)
	}
	return nil
}

// enqueueResume bumps tasks.attempts (this is one more dispatch attempt,
// whether the task is being resumed cleanly or auto-retried after a W1
// receipt-less interruption - see Machine.Transition's own doc comment
// for why a same-status redispatch bumps attempts directly here rather
// than through a status transition, since the task never leaves
// 'executing' in either case) and writes the resume row. The task's OWN
// status is left at 'executing' throughout - nothing here changes it.
func (r *Resume) enqueueResume(ctx context.Context, traceID, taskID string) error {
	ts := r.nowRFC3339()
	if _, err := r.store.IncrementTaskAttempts(ctx, sqlcgen.IncrementTaskAttemptsParams{UpdatedAt: ts, ID: taskID}); err != nil {
		return fmt.Errorf("increment attempts: %w", err)
	}
	return writeOutboxResumeRow(ctx, r.outbox, r.now, traceID, taskID)
}
