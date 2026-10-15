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
	"errors"
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
	// MsgResumeCapExceededFmt (FIX-I-2): %s=task_id, %d=crash_resumes,
	// %s=task_id. The task's worker kept ending without finishing and the
	// resume scan's crash re-dispatch budget (task_max_crash_resumes) ran
	// out.
	MsgResumeCapExceededFmt = "Görev %s: işçi süreci %d kez yarıda kesildi, otomatik devam limiti doldu — 'kahya task resolve %s' ile karar ver."
)

// Notification event kinds this file's blocked_user path ledgers via
// Notifier.Notify (which itself ledgers a `kind` row carrying the
// message - see kahyad/internal/notify.JSONLNotifier.Notify).
const (
	EventW1RetryCapExceeded = "task.w1_retry_cap_exceeded"
	EventW2W3Receiptless    = "task.w2w3_receiptless_blocked"
	EventResumeCapExceeded  = "task.resume_cap_exceeded"
)

// defaultW1MaxAuto mirrors config.Config's own default (kept as a small,
// local literal - this package does not import kahyad/internal/config,
// to avoid a dependency an internal state-machine package has no other
// reason to take; NewResume's caller (main.go) always threads the real
// configured value through).
const defaultW1MaxAuto = 3

// defaultMaxCrashResumes mirrors config.Config.TaskMaxCrashResumes' default
// (owner decision G10-2: 3 - the first spawn plus 3 crash resumes = 4 runs);
// main.go threads the configured value through SetMaxCrashResumes.
const defaultMaxCrashResumes = 3

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

// AnyLive composes LiveCheckers: a task is live when ANY of them reports it
// live (nil entries are skipped). main.go passes the worker registry
// (LiveRegistry: a worker process is running) and *Receipts (one of the
// task's side effects is executing inside kahyad right now, FIX-I-12) - the
// resume scan must leave a task alone in either case.
func AnyLive(checkers ...LiveChecker) LiveChecker {
	var live []LiveChecker
	for _, c := range checkers {
		if c != nil {
			live = append(live, c)
		}
	}
	return anyLive(live)
}

type anyLive []LiveChecker

func (a anyLive) IsLive(taskID string) bool {
	for _, c := range a {
		if c.IsLive(taskID) {
			return true
		}
	}
	return false
}

// laneSecret mirrors kahyad/internal/spawn.LaneSecret (tasks.lane /
// envelope.lane for a W3-08 secret-lane task) - duplicated rather than
// imported, the convention spawn's own envelope.go follows for router;
// TestLaneSecretMatchesSpawn pins the two together.
const laneSecret = "secret"

// Resumable reports whether the outbox dispatcher could ever redispatch a
// worker for t (FIX-I-14 review): only a task row that stored the envelope
// it was first handed to a worker with (POST /v1/task's cloud path), and
// never a secret-lane row (W3-08: answered on-device, never by a worker -
// kahyad/internal/outbox refuses such a row even when it stored an
// envelope). A locally answered task (secret-lane or downgrade-routed)
// and the briefing's one-shot session store no envelope. ParkOrGiveUp and
// kahyad's budget pause (kahyad/internal/server.NewTaskProxy) fail a
// non-resumable task instead of parking or blocking it - nothing could
// ever resume it, so it would sit non-terminal forever.
func Resumable(t sqlcgen.Task) bool {
	return t.Envelope.Valid && t.Envelope.String != "" && t.Lane != laneSecret
}

// OutboxEnqueuer is the narrow outbox-table surface enqueueResume,
// CloudRetry.park and Resolver.Retry need. *sqlcgen.Queries satisfies this
// directly.
//
// Every row written through it carries FixedNanoRFC3339 timestamps
// (writeOutboxResumeRowAt is the only writer): kahyad/internal/outbox
// compares available_at with tasks.next_retry_at as plain text (FIX-I-8's
// stale-row guard), which is only chronological for that fixed-width form.
type OutboxEnqueuer interface {
	InsertOutboxRow(ctx context.Context, arg sqlcgen.InsertOutboxRowParams) (sqlcgen.Outbox, error)
	// CountPendingOutboxRowsByTask (FIX-I-8) counts the task's undelivered,
	// uncanceled rows - enqueueResume's dedupe.
	CountPendingOutboxRowsByTask(ctx context.Context, taskID sql.NullString) (int64, error)
	// CancelOutboxRowsByTask (FIX-I-8) cancels the task's undelivered rows -
	// Resolver.Retry clears every older pending row (the park's future one
	// included) before writing its own.
	CancelOutboxRowsByTask(ctx context.Context, arg sqlcgen.CancelOutboxRowsByTaskParams) (int64, error)
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
	// maxCrashResumes caps the resume scan's own crash re-dispatches per
	// task (tasks.crash_resumes, FIX-I-2) - see SetMaxCrashResumes.
	maxCrashResumes int
	now             func() time.Time
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
		notifier: notifier, live: live, w1MaxAuto: w1MaxAuto,
		maxCrashResumes: defaultMaxCrashResumes, now: time.Now,
	}
}

// SetMaxCrashResumes sets the cap on the resume scan's crash re-dispatches
// per task (config task_max_crash_resumes, FIX-I-2): a task whose
// tasks.crash_resumes already reached n is moved to blocked_user instead of
// being enqueued again. n <= 0 restores defaultMaxCrashResumes.
func (r *Resume) SetMaxCrashResumes(n int) {
	if n <= 0 {
		n = defaultMaxCrashResumes
	}
	r.maxCrashResumes = n
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
//   - Otherwise the outcome is decided from EVERY listed receipt-less row
//     (FIX-FINAL resume-scan-ignores-older-receiptless-w2: not only the
//     newest row - FIX-I-4's ErrReceiptPersistFailed can leave an OLDER
//     W2/W3 row 'executing' while the task continues) before anything is
//     written:
//   - Any receipt-less row is class W2/W3 -> blocked_user + the exact
//     Turkish W2/W3 string naming the newest such row's tool. No
//     auto-retry at all (HANDOFF §6 W4: "yalniz W1 oto-tekrar, W2/W3
//     asla").
//   - Otherwise (all W1) -> count every attempt ever made for each
//     distinct (task_id, tool_name, args_hash) triple; any past
//     task.retry.w1_max_auto -> blocked_user + the exact Turkish W1-cap
//     string for the newest such row; all within the cap -> requeue for
//     resume (auto-retry).
//   - A block is made durable FIRST (the blocked_user transition), then
//     every row is marked failed, then the notice is sent; a requeue
//     marks every row first and enqueues only when all marks landed
//     (FIX-FINAL resume-scan-partial-mark-loses-w2-block). Any failure
//     leaves the rows a later scan needs to reach the same decision.
//
// FIX-I-2: every "requeue" outcome above first passes the crash-resume cap
// (requeueOrBlock): a task whose crash re-dispatches (tasks.crash_resumes)
// already reached task_max_crash_resumes moves to blocked_user with
// MsgResumeCapExceededFmt instead of being enqueued again - so a tool-less
// task whose worker keeps dying can no longer loop forever. The receipt-less
// W1-cap and W2/W3 decisions are taken first, so their own byte-exact
// notices keep priority (a W2/W3 call never auto-retries anyway).
func (r *Resume) ProcessTask(ctx context.Context, t sqlcgen.Task) error {
	traceID := t.TraceID

	rows, err := r.toolCalls.ListReceiptlessToolCalls(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list receipt-less tool_calls: %w", err)
	}
	if len(rows) == 0 {
		return r.requeueOrBlock(ctx, t)
	}

	// FIX-FINAL resume-scan-partial-mark-loses-w2-block: decide from the
	// listed rows BEFORE writing anything, make a block durable FIRST, and
	// only then mark the rows failed. Marking first (and stopping at the
	// first failed mark) left a newer W2/W3 row already 'failed' when a
	// later mark or the transition failed, so the next scan saw only the
	// remaining W1 rows and auto-requeued the task: a W2/W3 interruption
	// auto-retried on an error path (HANDOFF §6 W4 "W2/W3 asla"). Now a
	// failed transition leaves every row receipt-less for the next scan
	// (same rows -> same block), and once the task is blocked_user it is
	// outside the scan whatever a later mark does.
	//
	// rows are newest first (ListReceiptlessToolCalls ORDER BY id DESC).
	for _, row := range rows {
		if row.Class != string(ClassW1) {
			return r.blockThenMark(ctx, traceID, t.ID, rows, func() error {
				return r.notifyReceiptless(ctx, traceID, t.ID, row.ToolName)
			})
		}
	}

	// All W1: CountToolCallAttempts counts every row of the triple whatever
	// its status, so it is the same before and after the marks.
	type triple struct{ tool, argsHash string }
	counted := make(map[triple]bool, len(rows))
	for _, row := range rows {
		k := triple{row.ToolName, row.ArgsHash}
		if counted[k] {
			continue
		}
		counted[k] = true
		attempts, err := r.toolCalls.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{
			TaskID: t.ID, ToolName: row.ToolName, ArgsHash: row.ArgsHash,
		})
		if err != nil {
			return fmt.Errorf("count tool_calls attempts: %w", err)
		}
		if int(attempts) > r.w1MaxAuto {
			tool := row.ToolName
			return r.blockThenMark(ctx, traceID, t.ID, rows, func() error {
				return r.notifyW1CapExceeded(ctx, traceID, t.ID, tool, attempts)
			})
		}
	}

	// Requeue: every row must be marked failed first; on any failed mark
	// nothing is enqueued and the next scan (only W1 rows can remain)
	// recomputes the outcome.
	if err := r.markAllFailed(ctx, rows); err != nil {
		return err
	}
	return r.requeueOrBlock(ctx, t)
}

// blockThenMark is ProcessTask's block path: transition to blocked_user
// (on error nothing is marked - fail-closed), then mark every receipt-less
// row failed (all attempted, errors joined), then send the notice even if
// a mark failed. Do not put the marks and Machine.Transition in one tx:
// the transition's ledger write needs the single pooled connection.
func (r *Resume) blockThenMark(ctx context.Context, traceID, taskID string, rows []sqlcgen.ToolCall, notify func() error) error {
	if err := r.machine.Transition(ctx, traceID, taskID, StatusBlockedUser); err != nil {
		return fmt.Errorf("transition to blocked_user: %w", err)
	}
	markErr := r.markAllFailed(ctx, rows)
	return errors.Join(markErr, notify())
}

// markAllFailed marks every row failed, attempting all of them.
func (r *Resume) markAllFailed(ctx context.Context, rows []sqlcgen.ToolCall) error {
	var errs []error
	for _, row := range rows {
		if err := r.toolCalls.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{
			FinishedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: row.ID,
		}); err != nil {
			errs = append(errs, fmt.Errorf("mark tool_calls failed (id=%d): %w", row.ID, err))
		}
	}
	return errors.Join(errs...)
}

// requeueOrBlock is ProcessTask's single requeue point (FIX-I-2): within the
// crash-resume cap the task is enqueued again (enqueueResume counts it);
// at the cap an 'executing' task moves to blocked_user once. A task that is
// no longer 'executing' (ProcessTask is exported; Scan only ever passes
// 'executing' rows) is never blocked or notified a second time.
//
// FIX-FINAL resume-scan-blocks-redispatch-in-not-live-window: a pending
// (or claimed, not yet delivered) resume row already owns the task's next
// attempt (FIX-I-8), so nothing is decided while one exists - checked
// BEFORE the cap. Checking the cap first blocked a task whose last allowed
// resume row the dispatcher had claimed and was spawning (executing, not
// yet live) - the worker then ran anyway while the task sat in
// blocked_user with a cap notice - and blocked a task whose row merely
// waited behind a busy dispatcher. The cap still fires on the first scan
// after the last row is delivered.
func (r *Resume) requeueOrBlock(ctx context.Context, t sqlcgen.Task) error {
	pending, err := r.outbox.CountPendingOutboxRowsByTask(ctx, sql.NullString{String: t.ID, Valid: true})
	if err != nil {
		return fmt.Errorf("count pending outbox rows: %w", err)
	}
	if pending > 0 {
		return nil
	}
	if int(t.CrashResumes) >= r.maxCrashResumes {
		if t.Status != StatusExecuting {
			return nil
		}
		return r.blockOnResumeCapExceeded(ctx, t.TraceID, t.ID, t.CrashResumes)
	}
	return r.enqueueResume(ctx, t.TraceID, t.ID)
}

// notifyW1CapExceeded is the W1-past-cap notice (the task is already
// blocked_user, blockThenMark): the exact Turkish string.
func (r *Resume) notifyW1CapExceeded(ctx context.Context, traceID, taskID, tool string, attempts int64) error {
	msg := fmt.Sprintf(MsgW1RetryCapExceededFmt, taskID, tool, attempts, taskID)
	return r.notify(ctx, traceID, EventW1RetryCapExceeded, msg, taskID, tool, attempts)
}

// blockOnResumeCapExceeded implements the crash-resume cap (FIX-I-2):
// transition the task to blocked_user and notify with the exact Turkish
// string (the notifier ledgers EventResumeCapExceeded with the message).
func (r *Resume) blockOnResumeCapExceeded(ctx context.Context, traceID, taskID string, crashResumes int64) error {
	if err := r.machine.Transition(ctx, traceID, taskID, StatusBlockedUser); err != nil {
		return fmt.Errorf("transition to blocked_user: %w", err)
	}
	if r.notifier == nil {
		return nil
	}
	msg := fmt.Sprintf(MsgResumeCapExceededFmt, taskID, crashResumes, taskID)
	return r.notifier.Notify(ctx, traceID, EventResumeCapExceeded, msg, map[string]any{
		"task_id": taskID, "crash_resumes": crashResumes,
	})
}

// notifyReceiptless is the W2/W3-receipt-less notice (the task is already
// blocked_user, blockThenMark): the exact Turkish string. There is no
// attempt count in this message - there is no cap, because there is no
// auto-retry at all for W2/W3 (HANDOFF §6 W4 ⚑).
func (r *Resume) notifyReceiptless(ctx context.Context, traceID, taskID, tool string) error {
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
		// W6-03: back-links this row to taskID so the outbox claim query's
		// own defense-in-depth join (ListDueOutboxRows) and the halt
		// executor's CancelOutboxRowsByTask can both find it directly - see
		// migrations/0015_halt_semantics.sql's own doc comment.
		TaskID: sql.NullString{String: taskID, Valid: true},
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
// FIX-I-2: it also bumps tasks.crash_resumes - this is the one path that
// counts toward task_max_crash_resumes (cloud-park redispatches go through
// Machine.Transition and never touch it).
//
// FIX-I-8: while a resume row for the task is still pending (undelivered,
// not canceled - a row the dispatcher has claimed but not finished counts
// too) nothing is enqueued and nothing is bumped: that row already owns the
// next attempt. No ledger or log line either (the scan runs every tick).
func (r *Resume) enqueueResume(ctx context.Context, traceID, taskID string) error {
	pending, err := r.outbox.CountPendingOutboxRowsByTask(ctx, sql.NullString{String: taskID, Valid: true})
	if err != nil {
		return fmt.Errorf("count pending outbox rows: %w", err)
	}
	if pending > 0 {
		return nil
	}
	ts := r.nowRFC3339()
	if _, err := r.store.IncrementTaskAttempts(ctx, sqlcgen.IncrementTaskAttemptsParams{UpdatedAt: ts, ID: taskID}); err != nil {
		return fmt.Errorf("increment attempts: %w", err)
	}
	if _, err := r.store.IncrementTaskCrashResumes(ctx, sqlcgen.IncrementTaskCrashResumesParams{UpdatedAt: ts, ID: taskID}); err != nil {
		return fmt.Errorf("increment crash_resumes: %w", err)
	}
	return writeOutboxResumeRow(ctx, r.outbox, r.now, traceID, taskID)
}
