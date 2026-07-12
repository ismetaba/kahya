// Package task implements the W4-02 task durability state machine and
// the idempotency/receipt lifecycle around every side-effectful tool
// execution (HANDOFF §6 W4 ⚑: "idempotency/makbuz semantigi (intent ->
// executing -> receipt); makbuzsuz executing'de yalniz W1 oto-tekrar,
// W2/W3 asla"). This file (machine.go) is the state machine
// (tasks.status); receipts.go is the tool_calls intent/executing/receipt
// lifecycle + idempotent replay; resume.go is the crash-recovery scan
// that ties the two together (kahyad startup + a periodic tick).
//
// kahyad/internal/outbox.Dispatcher (a sibling package) is the
// redelivery loop that actually re-spawns a worker for a row this
// package enqueues - see EnqueueResume's own doc comment for why the
// dependency runs in that direction (task never imports outbox, so no
// import cycle).
package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store/sqlcgen"
)

// Status values - tasks.status's CHECK constraint enum
// (migrations/0007_task_durability.sql), verbatim from the task spec.
// 'bekliyor-yeniden-deneme' is POPULATED by W4-04 (cloud-error taxonomy);
// 'user_halted' SEMANTICS (kill process-group, invalidate approvals) are
// W6-03. Both enum values - and the transition edges that lead to/from
// them below - exist here now so those two later tasks are pure logic
// against an already-shaped state machine.
const (
	StatusIntent      = "intent"
	StatusExecuting   = "executing"
	StatusRetryWait   = "bekliyor-yeniden-deneme"
	StatusBlockedUser = "blocked_user"
	StatusUserHalted  = "user_halted"
	StatusDone        = "done"
	StatusFailed      = "failed"
)

// allowedTransitions is the COMPLETE legal-transition table (task spec
// step 2, verbatim): intent->executing; executing->{done, failed,
// blocked_user, bekliyor-yeniden-deneme, user_halted};
// bekliyor-yeniden-deneme->{executing, user_halted};
// blocked_user->{executing, failed, user_halted}. Any (from, to) pair not
// listed here - including every edge OUT of done/failed/user_halted, and
// user_halted->executing specifically (W6-03's whole point: a halted task
// is PERMANENTLY excluded from resume/retry) - is illegal. A from state
// with no entry at all (done, failed, user_halted) has zero legal
// outbound edges, which is exactly right: they are this machine's
// terminal states.
var allowedTransitions = map[string]map[string]bool{
	StatusIntent: {
		StatusExecuting: true,
	},
	StatusExecuting: {
		StatusDone:        true,
		StatusFailed:      true,
		StatusBlockedUser: true,
		StatusRetryWait:   true,
		StatusUserHalted:  true,
	},
	StatusRetryWait: {
		StatusExecuting:  true,
		StatusUserHalted: true,
	},
	StatusBlockedUser: {
		StatusExecuting:  true,
		StatusFailed:     true,
		StatusUserHalted: true,
	},
}

// IsLegalTransition reports whether from->to is one of the edges
// allowedTransitions lists. Exported so a caller (e.g. a future CLI
// preflight check) can ask without actually attempting the transition;
// Machine.Transition itself is the only place that ENFORCES it.
func IsLegalTransition(from, to string) bool {
	return allowedTransitions[from][to]
}

// ErrIllegalTransition is returned by Machine.Transition for any (from,
// to) pair IsLegalTransition rejects. The illegal attempt is ALWAYS
// ledgered (event=task.illegal_transition) before this is returned - see
// Transition's own doc comment.
var ErrIllegalTransition = errors.New("task: illegal status transition")

// ErrLostTransitionRace is returned by Machine.Transition when its
// compare-and-set UPDATE (SetTaskStatus, BLOCKER 3 fix) affects zero rows:
// taskID's status was legal-and-read as `from`, but by the time the UPDATE
// ran, some OTHER concurrent Transition call had already moved it
// somewhere else - the classic lost-race outcome of "read status, then
// write based on it" without a WHERE-clause guard. The caller's own
// attempted transition never happened at all (tasks.status is untouched by
// it, and no task.transition ledger event was appended for it) - this is
// deliberately distinct from ErrIllegalTransition (a transition that was
// never legal in the first place, from any starting status) versus this
// error (a transition that WAS legal from the status just read, but lost a
// race to a concurrent one).
var ErrLostTransitionRace = errors.New("task: lost transition race (status changed concurrently)")

// Store is the narrow tasks-table persistence surface Machine needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly, with no
// adapter - the same pattern kahyad/internal/policy.Store already
// establishes for the autonomy ladder.
type Store interface {
	GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error)
	// SetTaskStatus is now an atomic compare-and-set (BLOCKER 3 fix):
	// Status_2 carries the FROM status Transition just read, so the UPDATE
	// itself only ever affects a row when that from-status still holds -
	// the returned rows-affected count is what lets Transition detect a
	// lost race instead of blindly overwriting a concurrent winner (see
	// Transition's own doc comment).
	SetTaskStatus(ctx context.Context, arg sqlcgen.SetTaskStatusParams) (int64, error)
	IncrementTaskAttempts(ctx context.Context, arg sqlcgen.IncrementTaskAttemptsParams) (int64, error)
	// SetTaskNextRetry writes tasks.next_retry_at (W4-04's CloudRetry.park
	// - see cloudretry.go's own doc comment).
	SetTaskNextRetry(ctx context.Context, arg sqlcgen.SetTaskNextRetryParams) error
	// ListExecutingTasks is resume.go's resume-scan candidate query
	// (kahyad startup + a periodic tick) - included here (rather than a
	// separate interface) so kahyad/internal/task.Resume can share this
	// same Store value with Machine without a second, overlapping
	// interface definition.
	ListExecutingTasks(ctx context.Context) ([]sqlcgen.Task, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// Ledger is the append-only events sink every transition (legal or
// illegal) writes to (HANDOFF §5 safety #4). *store.Store already has
// exactly this method shape (store.Store.LogEvent), so it satisfies this
// with no adapter code - mirroring kahyad/internal/policy.Ledger's
// identical seam one package over.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Ledger event kinds this file appends. Exported so tests (and any
// future caller, e.g. a metrics query) can assert against the exact
// string rather than a locally duplicated literal.
const (
	EventTransition        = "task.transition"
	EventIllegalTransition = "task.illegal_transition"
	// EventTransitionConflict fires when Transition loses the
	// compare-and-set race (ErrLostTransitionRace, BLOCKER 3 fix) - kept
	// distinct from EventIllegalTransition (a (from, to) pair that was
	// never legal at all) so a ledger reader can tell "this was never
	// allowed" apart from "this was allowed but a concurrent transition
	// won first".
	EventTransitionConflict = "task.transition_conflict"
)

// Machine is the W4-02 task status state machine: one per kahyad
// process, sharing the single *store.Store the rest of the daemon uses.
type Machine struct {
	store  Store
	ledger Ledger
	// now is time.Now by default; tests substitute a fixed clock.
	now func() time.Time
	// jsonl is the OPTIONAL JSONL sink every transition additionally logs
	// to, alongside the DB ledger event (HANDOFF §4 ⚑: "her satir trace_id
	// iceren JSONL" - the acceptance criterion "every task/tool state
	// transition ledger event carries the task's trace_id, JSONL log +
	// events rows agree" needs BOTH to exist and agree, not only the DB
	// row). nil by default (every test/caller that never calls
	// SetJSONLLogger is unaffected, matching this codebase's usual
	// unwired-dependency posture) - main.go wires the real boot logger.
	jsonl *logx.Logger
}

// SetJSONLLogger wires jsonl as the additional JSONL sink every
// Transition call logs to (see the jsonl field's own doc comment). Safe
// to leave unset.
func (m *Machine) SetJSONLLogger(jsonl *logx.Logger) { m.jsonl = jsonl }

// NewMachine constructs a Machine. store/ledger may not be nil in
// production; tests pass a real temp *store.Store (kahyad/internal/store)
// or a fake.
func NewMachine(store Store, ledger Ledger) *Machine {
	return &Machine{store: store, ledger: ledger, now: time.Now}
}

// SetClock overrides Machine's clock (tests only).
func (m *Machine) SetClock(now func() time.Time) { m.now = now }

func (m *Machine) nowRFC3339() string { return m.now().UTC().Format(time.RFC3339Nano) }

// Transition drives task taskID's status from whatever it currently is to
// to, per allowedTransitions. Re-affirming the CURRENT status (from == to)
// is always a no-op (not itself a "transition" - no ledger event, no
// attempts bump, no error) so a caller never has to special-case "already
// there" against the legal-transition table. An illegal (from, to) pair
// ledgers EventIllegalTransition (task_id, from, to, trace_id) and returns
// ErrIllegalTransition WITHOUT writing tasks.status at all. A legal
// transition writes tasks.status, ledgers EventTransition (task_id, from,
// to, trace_id), and - specifically when to == StatusExecuting - also
// bumps tasks.attempts by one (every fresh dispatch INTO 'executing',
// whether from 'intent' on first spawn or from
// 'bekliyor-yeniden-deneme'/'blocked_user' on a later resume, is one more
// attempt at running this task's worker; the resume scan's own
// within-cap W1 receipt-less retry path, which never leaves 'executing'
// in the first place, bumps attempts directly instead - see resume.go).
//
// BLOCKER 3 fix: GetTaskByID's read and the SetTaskStatus write below are
// NOT protected by any lock spanning both - two concurrent Transition
// calls for the same taskID can both read the SAME stale `from` and both
// decide their own (from, to) pair is legal. What used to happen next was
// a blind `UPDATE tasks SET status=? WHERE id=?`: whichever caller's
// UPDATE ran LAST simply won, silently discarding the other's transition
// with no error to either caller (a torn, last-write-wins outcome - e.g.
// one caller legitimately moving executing->done while another legitimately
// moves the SAME task executing->user_halted would leave the task in
// WHICHEVER state happened to commit second, with the first caller having
// no idea it was overwritten). SetTaskStatus is now an atomic
// compare-and-set (`UPDATE ... WHERE id=? AND status=<from>`): only the
// FIRST of any two concurrent Transition calls racing from the same stale
// `from` ever actually affects a row. The loser's rows-affected comes back
// 0 - it lost the race (some other transition already moved taskID's
// status away from `from` between this call's own GetTaskByID read and its
// UPDATE) - and Transition returns ErrLostTransitionRace WITHOUT appending
// a task.transition ledger event for it (it never happened), so tasks.status
// ends up as EXACTLY one of the two callers' intended destinations, never a
// blend of both and never silently the "wrong" one with no signal.
func (m *Machine) Transition(ctx context.Context, traceID, taskID, to string) error {
	t, err := m.store.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task: load %s: %w", taskID, err)
	}
	from := t.Status
	if from == to {
		return nil
	}

	if !IsLegalTransition(from, to) {
		m.ledgerRaw(ctx, traceID, EventIllegalTransition, map[string]any{
			"event": EventIllegalTransition, "task_id": taskID, "from": from, "to": to,
		})
		m.logJSONL(traceID, EventIllegalTransition, taskID, from, to)
		return fmt.Errorf("%w: %s -> %s (task %s)", ErrIllegalTransition, from, to, taskID)
	}

	now := m.nowRFC3339()
	affected, err := m.store.SetTaskStatus(ctx, sqlcgen.SetTaskStatusParams{
		Status: to, UpdatedAt: now, ID: taskID, Status_2: from,
	})
	if err != nil {
		return fmt.Errorf("task: set status %s -> %s (task %s): %w", from, to, taskID, err)
	}
	if affected == 0 {
		m.ledgerRaw(ctx, traceID, EventTransitionConflict, map[string]any{
			"event": EventTransitionConflict, "task_id": taskID, "from": from, "to": to,
		})
		m.logJSONL(traceID, EventTransitionConflict, taskID, from, to)
		return fmt.Errorf("%w: %s -> %s (task %s)", ErrLostTransitionRace, from, to, taskID)
	}
	if to == StatusExecuting {
		if _, err := m.store.IncrementTaskAttempts(ctx, sqlcgen.IncrementTaskAttemptsParams{UpdatedAt: now, ID: taskID}); err != nil {
			return fmt.Errorf("task: increment attempts (task %s): %w", taskID, err)
		}
	}

	m.ledgerRaw(ctx, traceID, EventTransition, map[string]any{
		"event": EventTransition, "task_id": taskID, "from": from, "to": to,
	})
	m.logJSONL(traceID, EventTransition, taskID, from, to)
	return nil
}

func (m *Machine) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if m.ledger == nil {
		return
	}
	_ = m.ledger.LogEvent(ctx, traceID, kind, payload)
}

// logJSONL writes the SAME event/task_id/from/to fields the DB ledger row
// just got, as one JSONL line scoped to traceID - see the jsonl field's
// own doc comment for why both must exist and agree.
func (m *Machine) logJSONL(traceID, kind, taskID, from, to string) {
	if m.jsonl == nil {
		return
	}
	m.jsonl.With(traceID).Info(kind, "task_id", taskID, "from", from, "to", to)
}
