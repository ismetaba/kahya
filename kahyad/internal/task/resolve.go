// resolve.go implements `kahya task resolve <id> --retry|--abort`'s
// server-side logic (task spec step 9): the human decision a
// blocked_user task (or a bekliyor-yeniden-deneme one) is waiting on.
//
// --retry re-dispatches the task: Machine.Transition moves it back to
// 'executing' and a fresh outbox resume row is enqueued
// (writeOutboxResumeRow, shared with resume.go). Approval
// tokens are one-time (W3-02) and are NEVER reused here - Resolver does
// not mint or touch any token at all; the interrupted tool call's
// receipt-less row was already marked 'failed' by the resume scan (see
// resume.go's ProcessTask), so when the resumed worker/session genuinely
// re-attempts that exact call, Receipts.Execute finds no 'receipt' row
// (only the 'failed' one) and proceeds through the tool's OWN normal
// policy-check + approval flow from scratch, minting a brand new token -
// this package never manufactures one.
//
// --abort ends the task outright: Machine.Transition moves it to
// 'failed' and ledgers accordingly (Transition's own ledger event).
package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// ErrTaskNotResolvable is returned by Retry/Abort when the task is not
// currently in a state `kahya task resolve` can act on at all (Machine.
// Transition already enforces the legal-edge table - blocked_user's own
// legal edges are {executing, failed, user_halted}, and
// bekliyor-yeniden-deneme's are {executing, user_halted} - so both
// --retry and --abort simply delegate to Transition and let
// ErrIllegalTransition surface for anything else; this wrapper exists so
// callers of THIS package need not know about ErrIllegalTransition by
// name to recognize "the task wasn't in a resolvable state").
var ErrTaskNotResolvable = errors.New("task: not in a state kahya task resolve can act on")

// Resolver implements `kahya task resolve <id> --retry|--abort`.
type Resolver struct {
	store   Store
	outbox  OutboxEnqueuer
	machine *Machine
	now     func() time.Time
}

// NewResolver constructs a Resolver.
func NewResolver(store Store, outbox OutboxEnqueuer, machine *Machine) *Resolver {
	return &Resolver{store: store, outbox: outbox, machine: machine, now: time.Now}
}

// SetClock overrides Resolver's clock (tests only).
func (rs *Resolver) SetClock(now func() time.Time) { rs.now = now }

// Retry re-dispatches taskID: transitions it back to 'executing'
// (Machine.Transition itself bumps tasks.attempts for this fresh
// dispatch - see its own doc comment) and enqueues a fresh outbox resume
// row. Returns ErrTaskNotResolvable (wrapping the underlying
// ErrIllegalTransition) if taskID's current status has no legal edge into
// 'executing' (e.g. it is already 'done'/'failed'/'user_halted').
//
// FIX-I-2: an explicit human --retry also resets tasks.crash_resumes - a
// fresh crash-resume budget (task_max_crash_resumes) for the resumed task.
//
// FIX-I-8: the user said "now", so every older pending resume row for the
// task - a parked task's future row at next_retry_at included - is canceled
// and next_retry_at is cleared before the one fresh row is written; the
// task is never redispatched a second time by a leftover row.
//
// FIX-FINAL resume-scan-reblocks-during-task-retry: the old order
// (transition first, then cancel / reset / write) left a window in which
// the task was 'executing', not live, with no pending row and
// crash_resumes still at the cap - a resume-scan tick there re-blocked it
// with a second cap notice, and Retry then wrote a row the dispatcher's
// redelivery guard dropped ("retry ok" for a task that never ran). The
// state the scan reads is now made right while the task is still blocked /
// parked (never a scan candidate), then it moves to 'executing', and the
// fresh row is written only when no row is pending yet (a scan landing
// after the transition enqueues the single row itself). Deliberate
// departure from the handbook's I-2/I-8 statement order.
func (rs *Resolver) Retry(ctx context.Context, traceID, taskID string) error {
	cur, err := rs.store.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task: load task %s: %w", taskID, err)
	}
	if cur.Status != StatusExecuting && !IsLegalTransition(cur.Status, StatusExecuting) {
		// Machine.Transition ledgers the illegal attempt; nothing is reset.
		if err := rs.machine.Transition(ctx, traceID, taskID, StatusExecuting); err != nil {
			if errors.Is(err, ErrIllegalTransition) {
				return fmt.Errorf("%w: %v", ErrTaskNotResolvable, err)
			}
			return err
		}
		// only a terminal status has no edge into executing, so this is
		// unreachable; if it ever happened, the steps below still run (the
		// second transition is a from==to no-op)
	}
	now := rs.now().UTC().Format(time.RFC3339Nano)
	if _, err := rs.outbox.CancelOutboxRowsByTask(ctx, sqlcgen.CancelOutboxRowsByTaskParams{
		CanceledAt: sql.NullString{String: now, Valid: true},
		TaskID:     sql.NullString{String: taskID, Valid: true},
	}); err != nil {
		return fmt.Errorf("task: cancel pending outbox rows (task %s): %w", taskID, err)
	}
	if err := rs.store.ResetTaskCrashResumes(ctx, sqlcgen.ResetTaskCrashResumesParams{
		UpdatedAt: now, ID: taskID,
	}); err != nil {
		return fmt.Errorf("task: reset crash_resumes (task %s): %w", taskID, err)
	}
	if err := rs.store.SetTaskNextRetry(ctx, sqlcgen.SetTaskNextRetryParams{
		NextRetryAt: sql.NullString{}, UpdatedAt: now, ID: taskID,
	}); err != nil {
		return fmt.Errorf("task: clear next_retry_at (task %s): %w", taskID, err)
	}
	if err := rs.machine.Transition(ctx, traceID, taskID, StatusExecuting); err != nil {
		if errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("%w: %v", ErrTaskNotResolvable, err)
		}
		return err
	}
	pending, err := rs.outbox.CountPendingOutboxRowsByTask(ctx, sql.NullString{String: taskID, Valid: true})
	if err != nil {
		return fmt.Errorf("task: count pending outbox rows (task %s): %w", taskID, err)
	}
	if pending > 0 {
		return nil // a resume scan landing after the transition already enqueued it
	}
	return writeOutboxResumeRow(ctx, rs.outbox, rs.now, traceID, taskID)
}

// Abort ends taskID outright: transitions it to 'failed'. Returns
// ErrTaskNotResolvable (wrapping ErrIllegalTransition) if taskID's
// current status has no legal edge into 'failed'.
func (rs *Resolver) Abort(ctx context.Context, traceID, taskID string) error {
	if err := rs.machine.Transition(ctx, traceID, taskID, StatusFailed); err != nil {
		if errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("%w: %v", ErrTaskNotResolvable, err)
		}
		return err
	}
	return nil
}
