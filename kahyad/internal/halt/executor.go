// Package halt implements the W6-03 emergency halt (⌥⎋ - HANDOFF §6 W6
// flag, verbatim):
//
//	Hammerspoon'dan kahyad'a 'halt' IPC -> worker process-group'u +
//	ilgili Docker konteynerleri oldurulur, gorev terminal user_halted
//	durumuna yazilir (session-resume ve outbox retry'dan kalici haric),
//	bekleyen tum onaylar gecersiz kilinir.
//
// Executor.HaltTask/HaltAll are called from kahyad/internal/server's own
// POST /halt handler. Every step runs in the fixed order the task spec
// lists and is BEST-EFFORT-COMPLETE: a single step's failure is logged
// (JSONL, with the task's own trace_id - HANDOFF §4 ⚑ "her satir trace_id
// iceren JSONL") and the sequence CONTINUES to every later step, never
// partial-aborts. This mirrors kahyad/internal/task.Resume's own
// "continue past individual failures" posture one package over.
//
// SCOPE: halt only ever touches TASK-SCOPED processes (a worker's own
// process group, containers labeled kahya.task_id=<id>) - it NEVER
// signals kahyad itself, launchd jobs, or the kahyad-supervised MLX
// helper processes (mlx_lm.server, mlx/embed's server.py). Those are
// shared daemon infrastructure with their own lifecycle (the §4 idle-TTL
// unload), entirely outside this package's reach: Executor never imports
// kahyad/internal/mlxsup or kahyad/internal/mlx, and the process-group
// kill below can only ever reach -pgid, never kahyad's own pid.
package halt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"syscall"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
	mcpshell "kahya/mcp/shell"
)

// EventTaskUserHalted is the ledger event kind HaltTask appends once per
// task it actually halts (W6-03 deliverable, verbatim: "Ledger event
// kinds: task.user_halted, approval.invalidated" - the second kind,
// approval.invalidated, is kahyad/internal/policy.EventApprovalInvalidated,
// appended by ApprovalInvalidator.InvalidateApprovalsForTask itself, once
// per invalidated pending_approvals row).
const EventTaskUserHalted = "task.user_halted"

// Store is the narrow tasks+outbox persistence surface Executor needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly, with no
// adapter.
type Store interface {
	GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error)
	// ListNonTerminalTasks is {"all":true}'s own candidate set (task spec
	// step 4).
	ListNonTerminalTasks(ctx context.Context) ([]sqlcgen.Task, error)
	SetTaskHaltedAt(ctx context.Context, arg sqlcgen.SetTaskHaltedAtParams) error
	// CancelOutboxRowsByTask is task spec step 3.4 ("cancel the task's
	// undelivered outbox rows").
	CancelOutboxRowsByTask(ctx context.Context, arg sqlcgen.CancelOutboxRowsByTaskParams) (int64, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// Machine is the narrow task-status-transition surface Executor needs -
// *kahyad/internal/task.Machine satisfies this directly. Transitioning to
// task.StatusUserHalted is already a legal edge from executing/
// bekliyor-yeniden-deneme/blocked_user (kahyad/internal/task.
// allowedTransitions, shaped by migrations/0007_task_durability.sql
// specifically so this later task would be pure logic against an
// already-shaped state machine); user_halted itself has ZERO legal
// outbound edges, which is exactly HaltTask's own idempotent-no-op guard
// below relies on.
type Machine interface {
	Transition(ctx context.Context, traceID, taskID, to string) error
}

var _ Machine = (*task.Machine)(nil)

// LiveRegistry is the narrow in-memory worker-pid lookup Executor needs -
// *kahyad/internal/task.LiveRegistry satisfies this directly. The
// IN-MEMORY pid, when present, is ALWAYS preferred over the persisted
// tasks.worker_pgid column (task spec step 1: "kills whatever pgid is
// recorded for the task - in-memory entry if present, worker_pgid from
// the DB otherwise").
type LiveRegistry interface {
	PID(taskID string) (int, bool)
}

var _ LiveRegistry = (*task.LiveRegistry)(nil)

// ApprovalInvalidator is the narrow approval-invalidation surface
// Executor needs - *kahyad/internal/policy.Engine satisfies this
// directly via InvalidateApprovalsForTask (that method's own doc comment
// covers both halves: consuming every not-yet-decided pending_approvals
// row for the task AND revoking every not-yet-consumed approval_tokens
// row for it, which together are what makes the halt binding on every
// approval surface at once - a stale CLI decide, a Hammerspoon card
// button, or a Telegram inline button all hit the SAME revoked token/
// consumed row and are denied).
type ApprovalInvalidator interface {
	InvalidateApprovalsForTask(ctx context.Context, traceID, taskID string) (int, error)
}

var _ ApprovalInvalidator = (*policy.Engine)(nil)

// ContainerKiller is the narrow per-task Docker-kill surface Executor
// needs - *kahya/mcp/shell.Runner.KillLabeled satisfies this directly.
// May be nil (no Docker shell tool wired at all): every container-kill
// step is then simply skipped, matching KillLabeled's own "skip silently
// if Docker is not running" contract one level up.
type ContainerKiller interface {
	KillLabeled(ctx context.Context, taskID string) error
}

var _ ContainerKiller = (*mcpshell.Runner)(nil)

// SpeechKiller is the narrow W6-05 speech-kill surface Executor needs -
// *kahyad/internal/notify.Speaker satisfies this directly via
// KillTaskSpeech. May be nil (no Speaker wired at all - every pre-W6-05
// test/caller): the speech-kill step is then simply skipped, mirroring
// ContainerKiller's identical "may be nil" degrade above. A `say` child
// Speaker starts is a task-scoped process this package would otherwise
// never know about at all (Speaker owns/starts it directly, never via
// spawn.Run, so it carries no tasks.worker_pgid column of its own) -
// KillTaskSpeech is this package's only way to reach it.
type SpeechKiller interface {
	KillTaskSpeech(ctx context.Context, taskID string) error
}

// Ledger is the append-only events sink HaltTask writes to (HANDOFF §5
// safety #4). *store.Store already has exactly this method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Executor implements the W6-03 halt sequence - see this package's own
// doc comment for the full HANDOFF §6 W6 flag it satisfies.
type Executor struct {
	store     Store
	machine   Machine
	live      LiveRegistry        // may be nil
	approvals ApprovalInvalidator // may be nil
	docker    ContainerKiller     // may be nil
	speech    SpeechKiller        // may be nil - see SetSpeechKiller
	ledger    Ledger              // may be nil
	// jsonl is the OPTIONAL JSONL sink every halt step additionally logs
	// to, alongside the DB ledger row (mirrors kahyad/internal/task.
	// Machine's own jsonl field - see that field's doc comment for why
	// both must exist and agree). nil by default.
	jsonl *logx.Logger
	// now is time.Now by default; tests substitute a fixed clock.
	now func() time.Time
}

// NewExecutor constructs an Executor. store/machine may not be nil in
// production; live/approvals/docker/ledger may be nil (each documented
// above as a "this step is then simply skipped" degrade, matching this
// codebase's usual unwired-dependency posture) - though a production
// wiring with a nil approvals would defeat the whole point of W6-03's
// approval-invalidation half, so main.go always wires a real
// *kahyad/internal/policy.Engine there.
func NewExecutor(store Store, machine Machine, live LiveRegistry, approvals ApprovalInvalidator, docker ContainerKiller, ledger Ledger) *Executor {
	return &Executor{store: store, machine: machine, live: live, approvals: approvals, docker: docker, ledger: ledger, now: time.Now}
}

// SetJSONLLogger wires jsonl as the additional JSONL sink every halt step
// logs to (see the jsonl field's own doc comment). Safe to leave unset.
func (x *Executor) SetJSONLLogger(l *logx.Logger) { x.jsonl = l }

// SetSpeechKiller wires x (W6-05) - see the SpeechKiller type's own doc
// comment. Safe to leave unset (the speech-kill step is then simply
// skipped).
func (x *Executor) SetSpeechKiller(s SpeechKiller) { x.speech = s }

// SetClock overrides Executor's clock (tests only).
func (x *Executor) SetClock(now func() time.Time) { x.now = now }

func (x *Executor) nowRFC3339() string { return x.now().UTC().Format(time.RFC3339Nano) }

// isTerminalStatus reports whether status is one of the task state
// machine's three terminal, zero-outbound-edge statuses
// (kahyad/internal/task.allowedTransitions has no entry at all for any of
// these three, by construction).
func isTerminalStatus(status string) bool {
	switch status {
	case task.StatusDone, task.StatusFailed, task.StatusUserHalted:
		return true
	default:
		return false
	}
}

// HaltTask halts taskID: best-effort, in the fixed order the task spec
// lists (SIGKILL the worker's process group -> docker kill its labeled
// containers -> tasks.status='user_halted'+halted_at -> cancel its
// undelivered outbox rows -> invalidate its pending approvals + revoke
// their tokens -> ledger). Every step after the initial task lookup logs
// and continues past its own failure rather than aborting the rest (task
// spec step 3: "halt must be best-effort-complete, never partial-abort").
//
// haltedNow reports whether this call actually performed a FRESH halt
// (the task existed and was not already terminal). false with a nil
// error covers BOTH a taskID that does not exist at all AND a taskID that
// is already terminal (done/failed/user_halted) - both are documented
// no-op successes (task spec step 8: "halting an already-terminal task is
// a no-op that logs and returns success - a panicked double-press of
// ⌥⎋ must never error or corrupt state"). A non-nil error means the
// initial GetTaskByID lookup itself failed for a reason OTHER than "not
// found" (a genuine DB problem) - every step reached after that lookup
// succeeds is unconditionally best-effort and never itself returns an
// error to this method's own caller.
func (x *Executor) HaltTask(ctx context.Context, taskID string) (haltedNow bool, err error) {
	t, err := x.store.GetTaskByID(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("halt: load task %s: %w", taskID, err)
	}

	// Every ledger/JSONL line this call emits from here on is scoped to
	// the TASK'S OWN trace_id - the same correlation id every other event
	// in this task's life (task.transition, tool_calls, outbox rows) is
	// already scoped to - never a fresh per-request trace_id, so a single
	// `kahya log --trace <id>` / ledger grep against the task's trace_id
	// finds its halt too.
	traceID := t.TraceID

	if isTerminalStatus(t.Status) {
		x.logJSONL(traceID, "halt.noop_already_terminal", taskID, t.Status)
		return false, nil
	}

	// Step 1: SIGKILL the worker's process GROUP (task spec step 3.1).
	x.killProcessGroup(traceID, t)

	// Step 1b: kill this task's in-flight `say` child, if any (W6-05) -
	// see SpeechKiller's own doc comment for why this is a SEPARATE step
	// from killProcessGroup above (Speaker's child carries no
	// tasks.worker_pgid of its own). nil (no Speaker wired at all) is a
	// documented degrade - the step is simply skipped.
	if x.speech != nil {
		if err := x.speech.KillTaskSpeech(ctx, taskID); err != nil {
			x.logJSONLErr(traceID, "halt.speech_kill_failed", taskID, err)
		} else {
			x.logJSONL(traceID, "halt.speech_killed", taskID, "")
		}
	}

	// Step 2: docker kill every kahya.task_id=<taskID>-labeled container
	// (task spec step 3.2) - skipped silently when no ContainerKiller is
	// wired, mirroring KillLabeled's own "no Docker daemon" degrade.
	if x.docker != nil {
		if err := x.docker.KillLabeled(ctx, taskID); err != nil {
			x.logJSONLErr(traceID, "halt.docker_kill_failed", taskID, err)
		} else {
			x.logJSONL(traceID, "halt.docker_kill", taskID, "")
		}
	}

	// Step 3: tasks.status='user_halted' (task spec step 3.3) + halted_at.
	// Machine.Transition ALSO ledgers its own task.transition row - see
	// that method's doc comment - kept SEPARATE from this package's own
	// EventTaskUserHalted below (the exact ledger `kind` string the W6-03
	// acceptance criterion greps for).
	if err := x.machine.Transition(ctx, traceID, taskID, task.StatusUserHalted); err != nil {
		x.logJSONLErr(traceID, "halt.transition_failed", taskID, err)
	}
	if err := x.store.SetTaskHaltedAt(ctx, sqlcgen.SetTaskHaltedAtParams{
		HaltedAt: sql.NullString{String: x.nowRFC3339(), Valid: true}, UpdatedAt: x.nowRFC3339(), ID: taskID,
	}); err != nil {
		x.logJSONLErr(traceID, "halt.halted_at_persist_failed", taskID, err)
	}

	// Step 4: cancel the task's undelivered outbox rows (task spec step
	// 3.4).
	if n, err := x.store.CancelOutboxRowsByTask(ctx, sqlcgen.CancelOutboxRowsByTaskParams{
		CanceledAt: sql.NullString{String: x.nowRFC3339(), Valid: true},
		TaskID:     sql.NullString{String: taskID, Valid: true},
	}); err != nil {
		x.logJSONLErr(traceID, "halt.outbox_cancel_failed", taskID, err)
	} else {
		x.logJSONL(traceID, "halt.outbox_canceled", taskID, fmt.Sprintf("%d", n))
	}

	// Step 5: invalidate pending approvals + revoke their one-time tokens
	// (task spec step 3.5) - also ledgers approval.invalidated per row
	// (ApprovalInvalidator.InvalidateApprovalsForTask's own doc comment).
	// nil approvals is a documented (though never-in-production) degrade.
	if x.approvals != nil {
		if n, err := x.approvals.InvalidateApprovalsForTask(ctx, traceID, taskID); err != nil {
			x.logJSONLErr(traceID, "halt.approvals_invalidate_failed", taskID, err)
		} else {
			x.logJSONL(traceID, "halt.approvals_invalidated", taskID, fmt.Sprintf("%d", n))
		}
	}

	// Step 6: ledger task.user_halted (task spec step 3.6 - the
	// approval.invalidated half is already ledgered per-row above, by
	// InvalidateApprovalsForTask itself).
	x.ledgerRaw(ctx, traceID, EventTaskUserHalted, map[string]any{
		"event": EventTaskUserHalted, "task_id": taskID,
	})
	x.logJSONL(traceID, EventTaskUserHalted, taskID, "")

	return true, nil
}

// HaltAll halts every task currently in a non-terminal status
// (ListNonTerminalTasks - task spec step 4: "{all:true} iterates every
// task in a non-terminal running state"). Best-effort per task: one
// task's HaltTask error is logged and does not stop the rest of the
// sweep. Returns how many tasks this call FRESHLY halted (HaltTask's own
// haltedNow semantics) - not simply len(candidates), since a candidate
// could concurrently reach a terminal status between the list and this
// call's own HaltTask (which would then correctly report haltedNow=false
// for it, not an error).
func (x *Executor) HaltAll(ctx context.Context) (int, error) {
	tasks, err := x.store.ListNonTerminalTasks(ctx)
	if err != nil {
		return 0, fmt.Errorf("halt: list non-terminal tasks: %w", err)
	}
	n := 0
	for _, t := range tasks {
		haltedNow, err := x.HaltTask(ctx, t.ID)
		if err != nil {
			x.logJSONLErr(t.TraceID, "halt.task_failed", t.ID, err)
			continue
		}
		if haltedNow {
			n++
		}
	}
	return n, nil
}

// killProcessGroup implements task spec step 3.1: SIGKILL the worker's
// entire process GROUP - not just its own pid - so a child it forked
// (e.g. `sleep 300 &`) dies with it. pgid resolution prefers the
// in-memory LiveRegistry (the daemon's own current-process bookkeeping)
// and falls back to the persisted tasks.worker_pgid column ONLY when no
// in-memory entry exists - exactly the case a daemon crash/restart
// produces (macOS has no PDEATHSIG, so an orphaned worker from before the
// crash keeps running with an empty in-memory registry - migrations/
// 0015_halt_semantics.sql's own doc comment). A dead pgid makes
// syscall.Kill fail ESRCH, which is logged and ignored (task spec step 1:
// "a dead pgid makes kill fail with ESRCH, which is logged and ignored"),
// never treated as a halt failure.
func (x *Executor) killProcessGroup(traceID string, t sqlcgen.Task) {
	pgid := 0
	source := ""
	if x.live != nil {
		if pid, ok := x.live.PID(t.ID); ok && pid > 0 {
			pgid, source = pid, "memory"
		}
	}
	if pgid == 0 && t.WorkerPgid.Valid && t.WorkerPgid.Int64 > 0 {
		pgid, source = int(t.WorkerPgid.Int64), "db"
	}
	if pgid == 0 {
		// No worker was ever recorded for this task (e.g. a briefing/
		// consolidation session, or a task that never got past 'intent') -
		// nothing to kill, not a failure.
		x.logJSONL(traceID, "halt.no_worker_pgid", t.ID, "")
		return
	}

	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		x.logJSONLErr(traceID, "halt.kill_process_group_failed", t.ID, err)
		return
	}
	x.logJSONL(traceID, "halt.kill_process_group", t.ID, source)
}

func (x *Executor) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if x.ledger == nil {
		return
	}
	_ = x.ledger.LogEvent(ctx, traceID, kind, payload)
}

// logJSONL logs event scoped to traceID, with task_id and (when non-
// empty) a free-form detail field - every halt step is logged JSONL with
// trace_id (task spec step 3's own parenthetical). No-op when no JSONL
// logger is wired (tests, and any caller that never calls
// SetJSONLLogger).
func (x *Executor) logJSONL(traceID, event, taskID, detail string) {
	if x.jsonl == nil {
		return
	}
	if detail == "" {
		x.jsonl.With(traceID).Info(event, "task_id", taskID)
		return
	}
	x.jsonl.With(traceID).Info(event, "task_id", taskID, "detail", detail)
}

func (x *Executor) logJSONLErr(traceID, event, taskID string, err error) {
	if x.jsonl == nil {
		return
	}
	x.jsonl.With(traceID).Warn(event, "task_id", taskID, "err", err.Error())
}
