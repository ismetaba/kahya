package halt

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-I-14: ⌥⎋ used to "halt" every completed local/secret-lane task (they
// sat at status 'intent' forever): one illegal intent->user_halted attempt,
// a halted_at stamp, a task.user_halted row and +1 in "N görev durduruldu"
// per completed task. The count now reflects real transitions only.

// TestHaltAllIgnoresCompletedLocalTasks: a task that walked the machine to
// 'done' (what a local/secret-lane task does after the fix) is never
// swept. A legacy row still at intent (state done - the 0021 backfill
// moves those, this one predates it) is a never-started task: the new
// intent->user_halted edge halts it legally, with no illegal_transition
// noise.
func TestHaltAllIgnoresCompletedLocalTasks(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	machine := task.NewMachine(st.Queries, st)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []string{"local-done", "legacy-intent"} {
		if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
			ID: id, TraceID: "trace-" + id, SessionID: sql.NullString{}, State: "done",
			UpdatedAt: now, CreatedAt: now, Lane: "secret",
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
	}
	for _, to := range []string{task.StatusExecuting, task.StatusDone} {
		if err := machine.Transition(ctx, "trace-local-done", "local-done", to); err != nil {
			t.Fatalf("Transition(%s): %v", to, err)
		}
	}

	ex := NewExecutor(st.Queries, machine, task.NewLiveRegistry(), nil, nil, st)
	n, err := ex.HaltAll(ctx)
	if err != nil {
		t.Fatalf("HaltAll: %v", err)
	}
	if n != 1 {
		t.Errorf("HaltAll() = %d, want 1 (only the never-started legacy row)", n)
	}
	if got, _ := st.Queries.GetTaskByID(ctx, "local-done"); got.Status != task.StatusDone || got.HaltedAt.Valid {
		t.Errorf("local-done = status %q halted_at %+v, want done and never stamped", got.Status, got.HaltedAt)
	}
	if got, _ := st.Queries.GetTaskByID(ctx, "legacy-intent"); got.Status != task.StatusUserHalted {
		t.Errorf("legacy-intent status = %q, want %q", got.Status, task.StatusUserHalted)
	}
	if k := countEventsByKind(t, st.DB(), task.EventIllegalTransition); k != 0 {
		t.Errorf("%s rows = %d, want 0", task.EventIllegalTransition, k)
	}
	if k := countEventsByTraceAndKind(t, st, "trace-local-done", EventTaskUserHalted); k != 0 {
		t.Errorf("%s rows for the done task = %d, want 0", EventTaskUserHalted, k)
	}
}

// racingMachine loses every transition race (a concurrent writer moved the
// task first).
type racingMachine struct{}

func (racingMachine) Transition(_ context.Context, _, taskID, to string) error {
	return fmt.Errorf("%w: executing -> %s (task %s)", task.ErrLostTransitionRace, to, taskID)
}

// TestHaltTaskCountsOnlySuccessfulTransitions: when the user_halted
// transition does not happen (lost race / illegal), the task is not
// reported halted, halted_at is not stamped and no task.user_halted row is
// written - but the remaining safety steps (outbox rows cancelled) still
// run.
func TestHaltTaskCountsOnlySuccessfulTransitions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	row := insertExecutingTask(t, st, "halt-race-1")
	ob := enqueueResumeRowForTask(t, st, row.ID)

	ex := NewExecutor(st.Queries, racingMachine{}, task.NewLiveRegistry(), nil, nil, st)
	haltedNow, err := ex.HaltTask(ctx, row.ID)
	if err != nil {
		t.Fatalf("HaltTask: %v", err)
	}
	if haltedNow {
		t.Error("haltedNow = true, want false (the transition never happened)")
	}
	if got, _ := st.Queries.GetTaskByID(ctx, row.ID); got.HaltedAt.Valid {
		t.Errorf("halted_at = %+v, want unset", got.HaltedAt)
	}
	if k := countEventsByTraceAndKind(t, st, row.TraceID, EventTaskUserHalted); k != 0 {
		t.Errorf("%s rows = %d, want 0", EventTaskUserHalted, k)
	}
	var canceled sql.NullString
	if err := st.DB().QueryRow(`SELECT canceled_at FROM outbox WHERE id = ?`, ob.ID).Scan(&canceled); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if !canceled.Valid {
		t.Error("outbox row not cancelled: the remaining halt steps must still run")
	}
}

func countEventsByKind(t *testing.T, db *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}
