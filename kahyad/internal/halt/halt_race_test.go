package halt

import (
	"context"
	"sync"
	"testing"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// interleavingStore runs hook once, right after the halt Machine's first
// read of the task - between its read and its compare-and-set, the window
// a concurrent transition on the shared connection really lands in.
type interleavingStore struct {
	*sqlcgen.Queries
	once sync.Once
	hook func()
}

func (s *interleavingStore) GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error) {
	t, err := s.Queries.GetTaskByID(ctx, id)
	s.once.Do(s.hook)
	return t, err
}

// TestHaltRetriesLostRaceAgainstNonTerminalWinner (FIX-FINAL halt-lost-
// cas-race-leaves-task-runnable): HaltTask treated a lost user_halted
// compare-and-set as final, assuming a terminal winner. A non-terminal
// winner (here a cloud park, executing -> retry-wait; the outbox redispatch
// and Resolver.Retry are the same) left the task runnable: the resume scan
// later ran it to completion after the user pressed the halt key. The halt
// now retries while the task is non-terminal: it ends user_halted, counted,
// stamped and ledgered once, and the resume scan never picks it up.
func TestHaltRetriesLostRaceAgainstNonTerminalWinner(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	row := insertExecutingTask(t, st, "halt-race-park")
	other := task.NewMachine(st.Queries, st)
	ms := &interleavingStore{Queries: st.Queries, hook: func() {
		if err := other.Transition(ctx, row.TraceID, row.ID, task.StatusRetryWait); err != nil {
			t.Errorf("concurrent park: %v", err)
		}
	}}
	ex := NewExecutor(st.Queries, task.NewMachine(ms, st), task.NewLiveRegistry(), nil, nil, st)
	haltedNow, err := ex.HaltTask(ctx, row.ID)
	if err != nil || !haltedNow {
		t.Fatalf("HaltTask = %v, %v; want true (the halt must win over a non-terminal winner)", haltedNow, err)
	}
	got, _ := st.Queries.GetTaskByID(ctx, row.ID)
	if got.Status != task.StatusUserHalted || !got.HaltedAt.Valid {
		t.Fatalf("task = status %q halted_at %+v, want user_halted and stamped", got.Status, got.HaltedAt)
	}
	if k := countEventsByTraceAndKind(t, st, row.TraceID, EventTaskUserHalted); k != 1 {
		t.Errorf("%s rows = %d, want 1", EventTaskUserHalted, k)
	}
	r := task.NewResume(st.Queries, st.Queries, st.Queries, task.NewMachine(st.Queries, st), nil, nil, 3)
	if n, err := r.Scan(ctx); err != nil || n != 0 {
		t.Fatalf("resume scan examined %d (%v), want 0 - a halted task is never resumed", n, err)
	}
}

// TestHaltLostRaceToTerminalWinnerIsNotCounted: when the winner really is
// terminal (the task finished), the halt is not counted and nothing is
// stamped.
func TestHaltLostRaceToTerminalWinnerIsNotCounted(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	row := insertExecutingTask(t, st, "halt-race-done")
	other := task.NewMachine(st.Queries, st)
	ms := &interleavingStore{Queries: st.Queries, hook: func() {
		if err := other.Transition(ctx, row.TraceID, row.ID, task.StatusDone); err != nil {
			t.Errorf("concurrent finish: %v", err)
		}
	}}
	ex := NewExecutor(st.Queries, task.NewMachine(ms, st), task.NewLiveRegistry(), nil, nil, st)
	haltedNow, err := ex.HaltTask(ctx, row.ID)
	if err != nil || haltedNow {
		t.Fatalf("HaltTask = %v, %v; want false (the task finished first)", haltedNow, err)
	}
	got, _ := st.Queries.GetTaskByID(ctx, row.ID)
	if got.Status != task.StatusDone || got.HaltedAt.Valid {
		t.Fatalf("task = status %q halted_at %+v, want done and never stamped", got.Status, got.HaltedAt)
	}
	if k := countEventsByTraceAndKind(t, st, row.TraceID, EventTaskUserHalted); k != 0 {
		t.Errorf("%s rows = %d, want 0", EventTaskUserHalted, k)
	}
}
