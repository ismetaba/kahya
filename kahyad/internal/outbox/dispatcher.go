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
//
// BLOCKER 2 fix (dispatcher re-claiming a still-running worker):
// processResume used to never check whether a live worker already existed
// for a claimed row's task, and a claimed row's lease was computed once
// at claim time and never renewed while spawn.Run blocked for the
// worker's entire runtime - a task that ran longer than one lease period
// got re-claimed (by this same Dispatcher's own next tick, or a
// concurrent one) and a SECOND live worker spawned for the same
// task/session. processResume now (a) skips re-spawning entirely when
// LiveRegistry reports the task already live, and (b) runs a heartbeat
// goroutine that renews the claimed row's lease every leaseDuration/3 for
// as long as spawn.Run blocks, so a long-running task's row is never
// re-claimed purely because its ORIGINAL lease elapsed while it was still
// genuinely running. ClaimAndDispatch also gained its own in-flight guard
// (inFlight) so an overlapping cron tick - robfig/cron/v3 starts a new
// goroutine per scheduled fire with no built-in "skip if still running"
// by default - can never run a SECOND claim pass concurrently with one
// this same Dispatcher instance is still processing.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
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
	// EventResumeSkippedLive fires when a claimed row's task ALREADY has a
	// live, kahyad-owned worker running (LiveRegistry.IsLive) - BLOCKER 2
	// fix. The row is left exactly as claimed (not marked delivered): the
	// worker that is actually running will mark it delivered itself once
	// spawn.Run returns for that original processResume call - see the
	// package doc comment.
	EventResumeSkippedLive = "outbox.resume_skipped_live"
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
	// RenewOutboxLease extends a claimed row's lease_until - the BLOCKER 2
	// fix's heartbeat goroutine (renewLeaseWhileRunning) calls this
	// periodically for as long as spawn.Run blocks on the re-spawned
	// worker, so a long-running task's row is never re-claimed purely
	// because its original lease (computed once at claim time) elapsed.
	RenewOutboxLease(ctx context.Context, arg sqlcgen.RenewOutboxLeaseParams) error
	// SetTaskWorkerPGID persists a redispatched worker's process-group id
	// (W6-03) - see resume.go's own SetTaskWorkerPGID doc comment
	// (kahyad/internal/task's mirror of the same interface method); this
	// package's own processResume call site below is the SECOND of the
	// two places a fresh worker pid needs recording (the first is
	// kahyad/internal/server's handleTask, the first-spawn path).
	SetTaskWorkerPGID(ctx context.Context, arg sqlcgen.SetTaskWorkerPGIDParams) error
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
//
// IsLive is BLOCKER 2's own addition: processResume consults it BEFORE
// ever re-spawning a worker, so a row re-claimed while its task's
// original worker is still genuinely running (e.g. after a lease elapsed
// mid-run) is skipped rather than given a second, concurrent worker.
type LiveRegistry interface {
	Register(taskID string, pid int)
	Unregister(taskID string)
	IsLive(taskID string) bool
}

var _ LiveRegistry = (*task.LiveRegistry)(nil)

// AnthproxyOpener opens a fresh per-redispatch Anthropic forward-proxy
// listener for taskID (W4-04's fix for the gap this package's own doc
// comment used to note: "a genuinely resumed worker that needs to make a
// NEW model call would need its own fresh per-dispatch anthproxy.Proxy
// listener ... intentionally deferred"). Mirrors kahyad/internal/server's
// own per-task construction in handleTask - same governor/notifier/
// credential/egress-gate/cloud-retry-callback wiring, just invoked again
// at REDISPATCH time instead of at first-spawn time. Returns the base URL/
// token to set on the resumed envelope's spawn.Config and a close func to
// release the listener once spawn.Run returns. May be nil (pre-W4-04
// behavior: AnthropicBaseURL/APIKey stay empty on the resumed
// spawn.Config - a resumed cloud-lane task can never actually reach the
// cloud again; only a caller that has wired this can retry a genuinely
// cloud-lane task through the outbox).
type AnthproxyOpener func(ctx context.Context, taskID, traceID string) (baseURL, apiKey string, closeFn func() error, err error)

// Dispatcher is the W4-02 outbox redelivery loop.
type Dispatcher struct {
	store           Store
	ledger          Ledger
	machine         *task.Machine
	spawnCfg        spawn.Config
	live            LiveRegistry
	anthproxyOpener AnthproxyOpener

	batchSize     int
	leaseDuration time.Duration
	now           func() time.Time

	// inFlight is BLOCKER 2(c)'s overlap guard: true for the entire
	// duration of a ClaimAndDispatch call THIS Dispatcher instance is
	// already running. robfig/cron/v3 (kahyad/internal/scheduler) starts a
	// new goroutine per scheduled tick fire with no "skip if still
	// running" behavior of its own, so without this guard an
	// outbox_dispatch tick firing while the previous tick's
	// ClaimAndDispatch is still blocked (e.g. inside a long spawn.Run)
	// would start a SECOND, fully concurrent claim pass on the same
	// Dispatcher.
	inFlight atomic.Bool
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

// SetAnthproxyOpener wires opener - see AnthproxyOpener's own doc
// comment. Safe to leave unset (nil is the pre-W4-04 default posture).
func (d *Dispatcher) SetAnthproxyOpener(opener AnthproxyOpener) { d.anthproxyOpener = opener }

// SetClock overrides Dispatcher's clock (tests only).
func (d *Dispatcher) SetClock(now func() time.Time) { d.now = now }

func (d *Dispatcher) nowRFC3339() string { return d.now().UTC().Format(time.RFC3339Nano) }

// ClaimAndDispatch runs ONE claim pass: lists up to batchSize due rows,
// attempts to atomically claim each (ClaimOutboxRow's single-UPDATE
// guarantee - a row another concurrent dispatcher already claimed first
// affects 0 rows here and is silently skipped), and processes every row
// this call actually won. It returns how many rows this call claimed.
//
// BLOCKER 2(c) fix: if a PREVIOUS call to ClaimAndDispatch on this exact
// Dispatcher instance is still running (inFlight), this call returns
// (0, nil) immediately without listing or claiming anything - the
// overlap guard a periodic caller (kahyad/internal/scheduler's
// outbox_dispatch tick) needs, since robfig/cron/v3 does not itself skip
// a tick whose previous firing is still in flight.
func (d *Dispatcher) ClaimAndDispatch(ctx context.Context) (claimed int, err error) {
	if !d.inFlight.CompareAndSwap(false, true) {
		return 0, nil
	}
	defer d.inFlight.Store(false)

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
	// Project-review #6: also ack a row for an already-TERMINAL task (done/
	// failed). Without done/failed here the row fell through to
	// machine.Transition(->executing) below, which is illegal from a
	// terminal state, so it returned WITHOUT markDelivered and the row was
	// re-claimed every lease period forever — a zombie loop bumping
	// outbox.attempts and appending two events to the anchored ledger on
	// each pass. A done/failed task with an un-acked resume row is reachable
	// when a worker completed the task but was SIGKILLed before its own
	// processResume acked the row (or when a duplicate resume row was
	// enqueued for a still-executing task that later finished — see the
	// enqueueResume dedup in kahyad/internal/task/resume.go).
	if t.Status == task.StatusUserHalted || t.Status == task.StatusBlockedUser ||
		t.Status == task.StatusDone || t.Status == task.StatusFailed {
		d.ledgerRaw(ctx, t.TraceID, EventRedeliveryGuarded, map[string]any{
			"event": EventRedeliveryGuarded, "task_id": t.ID, "status": t.Status,
		})
		d.markDelivered(ctx, row.ID)
		return
	}

	// BLOCKER 2(a) fix: a live, kahyad-owned worker is ALREADY running for
	// this task - this row is a re-claim of one whose original lease
	// elapsed (or would have, absent the renewal below) while that worker
	// was still genuinely running, not a crashed/abandoned claim. Spawning
	// a SECOND worker for the same task/session here would be exactly the
	// double-execution this whole package exists to prevent. Leave the row
	// as claimed (NOT delivered) - the running worker's own processResume
	// call will mark it delivered itself once spawn.Run returns for THAT
	// call (see the package doc comment).
	if d.live != nil && d.live.IsLive(t.ID) {
		d.ledgerRaw(ctx, t.TraceID, EventResumeSkippedLive, map[string]any{
			"event": EventResumeSkippedLive, "task_id": t.ID, "outbox_id": row.ID,
		})
		return
	}

	env, err := d.buildResumeEnvelope(t)
	if err != nil {
		d.ledgerRaw(ctx, t.TraceID, "outbox.envelope_invalid", map[string]any{
			"event": "outbox.envelope_invalid", "task_id": t.ID, "err": err.Error(),
		})
		return
	}

	// W4-04: a row claimed while the task sits in bekliyor-yeniden-deneme
	// (kahyad/internal/task.CloudRetry.park's own outbox row, enqueued at
	// next_retry_at) must move back to 'executing' before redispatch -
	// bekliyor-yeniden-deneme's only legal edges are {executing,
	// user_halted} (kahyad/internal/task.allowedTransitions), so without
	// this the eventual done/failed transition below would be illegal and
	// the task would stay stuck parked forever even after a genuinely
	// successful resumed call. A no-op (Machine.Transition's own from==to
	// short-circuit, no ledger row, no attempts bump) for the ordinary
	// crash-recovery case where the task was already 'executing' the
	// whole time (resume.go's enqueueResume never changes status either).
	if err := d.machine.Transition(ctx, t.TraceID, t.ID, task.StatusExecuting); err != nil {
		d.ledgerRaw(ctx, t.TraceID, "outbox.transition_failed", map[string]any{
			"event": "outbox.transition_failed", "task_id": t.ID, "err": err.Error(),
		})
		// Project-review #6: an ILLEGAL transition here means the task raced
		// into a terminal state between the guard check above and now — the
		// row can never legally dispatch, so ack it instead of leaving it to
		// be re-claimed forever. (Any other transition error is a transient
		// DB fault; leave the row for lease-expiry re-claim as before.)
		if errors.Is(err, task.ErrIllegalTransition) {
			d.markDelivered(ctx, row.ID)
		}
		return
	}

	if d.live != nil {
		defer d.live.Unregister(t.ID)
	}

	// W4-04: open a FRESH per-redispatch Anthropic forward-proxy listener
	// for this exact call - see AnthproxyOpener's own doc comment for why
	// this is needed at all (a resumed cloud-lane task cannot make ANY
	// model call without one). spawnCfg is a per-call COPY of d.spawnCfg
	// (never mutating the shared Dispatcher-wide config), with
	// AnthropicBaseURL/APIKey overridden - exactly the two fields
	// kahyad/internal/server's own per-task construction sets, nothing
	// else differs from d.spawnCfg.
	spawnCfg := d.spawnCfg
	if d.anthproxyOpener != nil {
		baseURL, apiKey, closeProxy, err := d.anthproxyOpener(ctx, t.ID, t.TraceID)
		if err != nil {
			d.ledgerRaw(ctx, t.TraceID, "outbox.anthproxy_open_failed", map[string]any{
				"event": "outbox.anthproxy_open_failed", "task_id": t.ID, "err": err.Error(),
			})
			return
		}
		spawnCfg.AnthropicBaseURL = baseURL
		spawnCfg.APIKey = apiKey
		if closeProxy != nil {
			defer func() { _ = closeProxy() }()
		}
	}

	// BLOCKER 2(b) fix: renew this row's lease every leaseDuration/3 for as
	// long as spawn.Run (below) blocks, so a worker that runs longer than
	// one lease period is never re-claimed purely because the lease
	// ClaimOutboxRow set once, at claim time, elapsed while it was still
	// genuinely running - see renewLeaseWhileRunning's own doc comment.
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go d.renewLeaseWhileRunning(row.ID, heartbeatDone)

	outcome, runErr := spawn.Run(ctx, spawnCfg, env, spawn.Callbacks{
		OnStart: func(pid int) {
			if d.live != nil {
				d.live.Register(t.ID, pid)
			}
			// W6-03: persist the redispatched worker's process-group id
			// ALONGSIDE the in-memory registry above - see
			// kahyad/internal/server's handleTask OnStart callback (the
			// first-spawn path's mirror of this same write) for why. Best-
			// effort: a write failure here is logged, never fatal to the
			// redispatch itself.
			if err := d.store.SetTaskWorkerPGID(ctx, sqlcgen.SetTaskWorkerPGIDParams{
				WorkerPgid: sql.NullInt64{Int64: int64(pid), Valid: true}, UpdatedAt: d.nowRFC3339(), ID: t.ID,
			}); err != nil {
				d.ledgerRaw(ctx, t.TraceID, "outbox.worker_pgid_persist_failed", map[string]any{
					"event": "outbox.worker_pgid_persist_failed", "task_id": t.ID, "err": err.Error(),
				})
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

	// Not a clean success. The W4-04 cloud-retry callbacks
	// (anthproxy.OnCloudUnreachable -> task.CloudRetry.ParkOrGiveUp,
	// anthproxy.OnNonRetryableFailure -> task.CloudRetry.FailNonRetryable)
	// fire SYNCHRONOUSLY mid-spawn.Run, so by the time we get here the task
	// may already have been re-parked or moved to a terminal state. Reload
	// it and decide THIS claimed row's fate by that final status:
	//
	//   - bekliyor-yeniden-deneme (re-parked): ParkOrGiveUp already enqueued
	//     a FRESH outbox row scheduled at next_retry_at
	//     (1m/5m/15m/60m/hourly) that now owns the next attempt. THIS row
	//     must be marked delivered - otherwise its short claim-lease
	//     (leaseDuration, default 2m) re-claims the task far earlier than the
	//     park's own next_retry_at, bypassing the backoff schedule entirely
	//     and spawning a brand-new stale row on every subsequent failure.
	//   - done/failed/user_halted/blocked_user (terminal): nothing more to
	//     redeliver; mark delivered so it is never re-claimed.
	//   - still executing/intent (a genuine crash - the worker died with
	//     nothing having parked or failed the task): leave the row
	//     unacknowledged (task spec step 7) so lease expiry re-claims it,
	//     the original W4-02 at-least-once recovery path.
	cur, gerr := d.store.GetTaskByID(ctx, t.ID)
	if gerr != nil {
		// Can't determine the task's post-run state - leave the row for
		// lease-expiry re-claim rather than risk dropping a needed
		// redelivery (fail toward "retry", never toward "silently drop").
		d.ledgerRaw(ctx, t.TraceID, "outbox.task_reload_failed", map[string]any{
			"event": "outbox.task_reload_failed", "task_id": t.ID, "outbox_id": row.ID, "err": gerr.Error(),
		})
		return
	}
	switch cur.Status {
	case task.StatusRetryWait, task.StatusDone, task.StatusFailed,
		task.StatusUserHalted, task.StatusBlockedUser:
		d.markDelivered(ctx, row.ID)
	default:
		// still executing/intent: genuine crash, leave unacknowledged for
		// lease-expiry re-claim (ClaimOutboxRow already bumped
		// outbox.attempts for THIS claim regardless of outcome).
	}
}

// renewLeaseWhileRunning extends outboxID's lease every leaseDuration/3
// (BLOCKER 2(b) fix) until done is closed - processResume closes done via
// defer immediately once spawn.Run returns, which also stops this
// goroutine. Runs against context.Background() rather than processResume's
// own ctx: a renewal already in flight when processResume returns should
// still complete rather than being aborted mid-write (a renewal landing a
// moment late is harmless - see RenewOutboxLease's own dispatched_at IS
// NULL guard and the package doc comment's "worst case: one extra,
// otherwise-safe reclaim-then-skip cycle"), but this goroutine itself must
// never outlive done being closed.
func (d *Dispatcher) renewLeaseWhileRunning(outboxID int64, done <-chan struct{}) {
	interval := d.leaseDuration / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			leaseUntil := task.FixedNanoRFC3339(d.now().Add(d.leaseDuration))
			if err := d.store.RenewOutboxLease(context.Background(), sqlcgen.RenewOutboxLeaseParams{
				LeaseUntil: sql.NullString{String: leaseUntil, Valid: true}, ID: outboxID,
			}); err != nil {
				d.ledgerRaw(context.Background(), "", "outbox.lease_renew_failed", map[string]any{
					"event": "outbox.lease_renew_failed", "outbox_id": outboxID, "err": err.Error(),
				})
			}
		}
	}
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
