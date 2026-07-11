// Package outbox implements the W4-02 lease-based outbox redelivery loop
// (task spec step 7): it claims due rows kahyad/internal/task's resume
// scan (or `kahya task resolve <id> --retry`) enqueued, re-spawns the
// worker with the W12-07 envelope plus the task's own session_id +
// resume:true, and marks the row delivered once the worker reports
// success. A crashed/never-acknowledged claim's lease simply expires and
// gets re-claimed - by this dispatcher or another one, whichever gets
// there first (ClaimOutboxRow's single atomic UPDATE is what makes two
// concurrent dispatchers racing on the SAME row safe: only the first ever
// affects it).
//
// This package depends on kahyad/internal/task (for the status enum,
// Machine, and the outbox payload shape), never the other way around -
// kahyad/internal/task never imports this package, so there is no import
// cycle.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// Ledger event kinds this file appends.
const (
	// EventRedeliveryGuarded fires when a claimed row's task has already
	// moved to user_halted or blocked_user by the time the row is
	// processed (task spec step 7: "Never redeliver tasks in user_halted
	// or blocked_user - guard checked at claim time, not enqueue time").
	// The row is still marked delivered (there is nothing further to do
	// for it - redelivering would violate the guard on every future claim
	// too), it is simply never dispatched to a worker.
	EventRedeliveryGuarded = "outbox.redelivery_guarded"
	// EventResumeDispatched fires once per row this dispatcher actually
	// re-spawns a worker for (successfully claimed, task not
	// halted/blocked).
	EventResumeDispatched = "outbox.resume_dispatched"
)

// defaultBatchSize/defaultLeaseDuration are Dispatcher's own defaults
// (NewDispatcher); a caller (main.go) may override either via
// SetBatchSize/SetLeaseDuration.
const (
	defaultBatchSize     = 20
	defaultLeaseDuration = 2 * time.Minute
)

// Store is the narrow outbox+tasks persistence surface Dispatcher needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly, with no
// adapter.
type Store interface {
	ListDueOutboxRows(ctx context.Context, arg sqlcgen.ListDueOutboxRowsParams) ([]sqlcgen.Outbox, error)
	ClaimOutboxRow(ctx context.Context, arg sqlcgen.ClaimOutboxRowParams) (int64, error)
	MarkOutboxDelivered(ctx context.Context, arg sqlcgen.MarkOutboxDeliveredParams) error
	GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error)
	UpdateTaskSession(ctx context.Context, arg sqlcgen.UpdateTaskSessionParams) error
}

var _ Store = (*sqlcgen.Queries)(nil)

// Ledger is the append-only events sink this package writes to (HANDOFF
// §5 safety #4). *store.Store already has exactly this method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// LiveRegistry is the narrow subset of kahyad/internal/task.LiveRegistry
// Dispatcher needs, so a re-spawned worker's PID is visible to the SAME
// registry the resume scan's LiveChecker consults (and `kahya task show
// <id>` reads from) for the whole time it is actually running - not just
// the first time a task was ever spawned. Optional (nil is a documented
// no-op, matching this codebase's usual unwired-dependency posture).
type LiveRegistry interface {
	Register(taskID string, pid int)
	Unregister(taskID string)
}

var _ LiveRegistry = (*task.LiveRegistry)(nil)

// Dispatcher is the W4-02 outbox redelivery loop.
type Dispatcher struct {
	store    Store
	ledger   Ledger
	machine  *task.Machine
	spawnCfg spawn.Config
	live     LiveRegistry

	batchSize     int
	leaseDuration time.Duration
	now           func() time.Time
}

// NewDispatcher constructs a Dispatcher. spawnCfg is the SAME
// spawn.Config shape POST /v1/task's handler already builds
// (kahyad/internal/server's task.go) - Cmd/Socket/LogDir/
// AnthropicBaseURL/APIKey/MCPBridgePath/CredentialMode. live may be nil
// (see LiveRegistry's own doc comment).
func NewDispatcher(store Store, ledger Ledger, machine *task.Machine, spawnCfg spawn.Config, live LiveRegistry) *Dispatcher {
	return &Dispatcher{
		store: store, ledger: ledger, machine: machine, spawnCfg: spawnCfg, live: live,
		batchSize: defaultBatchSize, leaseDuration: defaultLeaseDuration, now: time.Now,
	}
}

// SetBatchSize overrides how many due rows one ClaimAndDispatch pass
// considers (default 20).
func (d *Dispatcher) SetBatchSize(n int) { d.batchSize = n }

// SetLeaseDuration overrides how long a claimed row's lease lasts before
// it becomes re-claimable again (default 2 minutes). Tests use a much
// shorter value to exercise lease-expiry re-claim without a real wait.
func (d *Dispatcher) SetLeaseDuration(d2 time.Duration) { d.leaseDuration = d2 }

// SetClock overrides Dispatcher's clock (tests only).
func (d *Dispatcher) SetClock(now func() time.Time) { d.now = now }

func (d *Dispatcher) nowRFC3339() string { return d.now().UTC().Format(time.RFC3339Nano) }

// ClaimAndDispatch runs ONE claim pass: lists up to batchSize due rows,
// attempts to atomically claim each (ClaimOutboxRow's single-UPDATE
// guarantee - a row another concurrent dispatcher already claimed first
// affects 0 rows here and is silently skipped), and processes every row
// this call actually won. It returns how many rows this call claimed.
func (d *Dispatcher) ClaimAndDispatch(ctx context.Context) (claimed int, err error) {
	// FixedNanoRFC3339, NOT nowRFC3339: available_at/lease_until are
	// compared with a plain SQL TEXT `<=`/`<` (ListDueOutboxRows/
	// ClaimOutboxRow) - see FixedNanoRFC3339's own doc comment for why a
	// trimmed time.RFC3339Nano value would make that comparison
	// occasionally wrong.
	now := task.FixedNanoRFC3339(d.now())
	rows, err := d.store.ListDueOutboxRows(ctx, sqlcgen.ListDueOutboxRowsParams{
		AvailableAt: sql.NullString{String: now, Valid: true},
		LeaseUntil:  sql.NullString{String: now, Valid: true},
		Limit:       int64(d.batchSize),
	})
	if err != nil {
		return 0, fmt.Errorf("outbox: list due rows: %w", err)
	}

	for _, row := range rows {
		leaseUntil := task.FixedNanoRFC3339(d.now().Add(d.leaseDuration))
		affected, cerr := d.store.ClaimOutboxRow(ctx, sqlcgen.ClaimOutboxRowParams{
			LeaseUntil: sql.NullString{String: leaseUntil, Valid: true}, ID: row.ID,
			LeaseUntil_2: sql.NullString{String: now, Valid: true},
		})
		if cerr != nil || affected == 0 {
			// Either a genuine DB error (best-effort: move on to the next
			// row rather than abort the whole pass) or another dispatcher
			// won the race for this exact row first - either way, this
			// dispatcher must not touch it.
			continue
		}
		claimed++
		d.processRow(ctx, row)
	}
	return claimed, nil
}

// processRow dispatches on row.Kind. Unknown kinds are ledgered and left
// as-is (the claimed lease will simply expire and be re-claimed later -
// there is no other kind defined yet as of W4-02, so this is purely
// defensive).
func (d *Dispatcher) processRow(ctx context.Context, row sqlcgen.Outbox) {
	switch row.Kind {
	case task.OutboxKindTaskResume:
		d.processResume(ctx, row)
	default:
		d.ledgerRaw(ctx, row.TraceID, "outbox.unknown_kind", map[string]any{
			"event": "outbox.unknown_kind", "outbox_id": row.ID, "kind": row.Kind,
		})
	}
}

// processResume implements the resume-row half of task spec step 7: load
// the task, apply the never-redeliver guard, build the resume envelope
// from the task's OWN originally-stored envelope (session_id + resume:true
// overridden - task_id/trace_id/prompt/model/lane/category all carried
// through UNCHANGED, task spec's own "resumed-task envelope carries the
// original session_id and trace_id" acceptance test), re-spawn the
// worker, and mark the row delivered on a clean exit 0 (leaving it
// unacknowledged - lease expiry re-claims - on anything else, per task
// spec step 7).
func (d *Dispatcher) processResume(ctx context.Context, row sqlcgen.Outbox) {
	var payload task.OutboxTaskResumePayload
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		d.ledgerRaw(ctx, row.TraceID, "outbox.payload_invalid", map[string]any{
			"event": "outbox.payload_invalid", "outbox_id": row.ID, "err": err.Error(),
		})
		return
	}

	t, err := d.store.GetTaskByID(ctx, payload.TaskID)
	if err != nil {
		d.ledgerRaw(ctx, row.TraceID, "outbox.task_load_failed", map[string]any{
			"event": "outbox.task_load_failed", "outbox_id": row.ID, "task_id": payload.TaskID, "err": err.Error(),
		})
		return
	}

	// Never-redeliver guard, checked at CLAIM time (task spec step 7),
	// not at the time this row was originally enqueued - a task can have
	// moved to blocked_user (the resume scan's own W1-cap/W2-W3 branch) or
	// user_halted (a later ⌥⎋, W6-03) any time between enqueue and this
	// claim.
	if t.Status == task.StatusUserHalted || t.Status == task.StatusBlockedUser {
		d.ledgerRaw(ctx, t.TraceID, EventRedeliveryGuarded, map[string]any{
			"event": EventRedeliveryGuarded, "task_id": t.ID, "status": t.Status,
		})
		d.markDelivered(ctx, row.ID)
		return
	}

	env, err := d.buildResumeEnvelope(t)
	if err != nil {
		d.ledgerRaw(ctx, t.TraceID, "outbox.envelope_invalid", map[string]any{
			"event": "outbox.envelope_invalid", "task_id": t.ID, "err": err.Error(),
		})
		return
	}

	if d.live != nil {
		defer d.live.Unregister(t.ID)
	}

	outcome, runErr := spawn.Run(ctx, d.spawnCfg, env, spawn.Callbacks{
		OnStart: func(pid int) {
			if d.live != nil {
				d.live.Register(t.ID, pid)
			}
		},
		OnSession: func(sessionID string) {
			if sessionID == "" {
				return
			}
			_ = d.store.UpdateTaskSession(ctx, sqlcgen.UpdateTaskSessionParams{
				SessionID: sql.NullString{String: sessionID, Valid: true}, UpdatedAt: d.nowRFC3339(), ID: t.ID,
			})
		},
	})

	d.ledgerRaw(ctx, t.TraceID, EventResumeDispatched, map[string]any{
		"event": EventResumeDispatched, "task_id": t.ID, "session_id": env.SessionID, "resume": env.Resume,
	})

	if runErr == nil && outcome.Status == spawn.StatusOK {
		if err := d.machine.Transition(ctx, t.TraceID, t.ID, task.StatusDone); err != nil {
			d.ledgerRaw(ctx, t.TraceID, "outbox.transition_failed", map[string]any{
				"event": "outbox.transition_failed", "task_id": t.ID, "err": err.Error(),
			})
		}
		d.markDelivered(ctx, row.ID)
		return
	}

	// Non-zero exit (or a spawn-level error) WITHOUT a terminal task
	// state: leave the row unacknowledged (task spec step 7) - lease
	// expiry drives the re-claim, and ClaimOutboxRow already incremented
	// outbox.attempts for THIS claim regardless of how it turns out.
}

func (d *Dispatcher) markDelivered(ctx context.Context, outboxID int64) {
	if err := d.store.MarkOutboxDelivered(ctx, sqlcgen.MarkOutboxDeliveredParams{
		DispatchedAt: sql.NullString{String: d.nowRFC3339(), Valid: true}, ID: outboxID,
	}); err != nil {
		d.ledgerRaw(ctx, "", "outbox.mark_delivered_failed", map[string]any{
			"event": "outbox.mark_delivered_failed", "outbox_id": outboxID, "err": err.Error(),
		})
	}
}

// ErrNoStoredEnvelope is returned by buildResumeEnvelope when the task
// row has no persisted envelope to resume from at all (should not happen
// in production - POST /v1/task always stores one at InsertTask time).
var ErrNoStoredEnvelope = errors.New("outbox: task has no stored envelope to resume from")

// buildResumeEnvelope reconstructs the spawn.Envelope to re-spawn t's
// worker with: the ORIGINAL envelope JSON persisted at first spawn
// (tasks.envelope), with SessionID/Resume overridden to reflect t's
// current stored session_id - task_id, trace_id, prompt, model, lane, and
// category all carry through UNCHANGED (this is the mechanism behind the
// task spec's own acceptance test: "envelope for a resumed task carries
// the original session_id and trace_id"). A task with no stored
// session_id yet (crashed before the worker ever reported one) gets
// resume:false - there is nothing to resume, so this is simply a fresh
// re-spawn of the same prompt.
func (d *Dispatcher) buildResumeEnvelope(t sqlcgen.Task) (spawn.Envelope, error) {
	if !t.Envelope.Valid || t.Envelope.String == "" {
		return spawn.Envelope{}, ErrNoStoredEnvelope
	}
	var env spawn.Envelope
	if err := json.Unmarshal([]byte(t.Envelope.String), &env); err != nil {
		return spawn.Envelope{}, fmt.Errorf("decode stored envelope: %w", err)
	}

	env.TaskID = t.ID
	env.TraceID = t.TraceID
	if t.SessionID.Valid && t.SessionID.String != "" {
		sid := t.SessionID.String
		env.SessionID = &sid
		env.Resume = true
	} else {
		env.SessionID = nil
		env.Resume = false
	}

	if err := env.Validate(); err != nil {
		return spawn.Envelope{}, fmt.Errorf("resume envelope: %w", err)
	}
	return env, nil
}

func (d *Dispatcher) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if d.ledger == nil {
		return
	}
	_ = d.ledger.LogEvent(ctx, traceID, kind, payload)
}
