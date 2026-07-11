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
	"errors"
	"fmt"
	"time"
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
func (rs *Resolver) Retry(ctx context.Context, traceID, taskID string) error {
	if err := rs.machine.Transition(ctx, traceID, taskID, StatusExecuting); err != nil {
		if errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("%w: %v", ErrTaskNotResolvable, err)
		}
		return err
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
