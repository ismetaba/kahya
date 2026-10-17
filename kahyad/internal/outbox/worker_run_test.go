package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-FINAL receipt-replay-per-run: every redispatch is the task's NEXT
// worker run (tasks.worker_run), counted before the worker exists, so the
// resumed session's tool calls are told apart from the earlier run's.

func bumpWorkerRun(t *testing.T, st *store.Store, taskID string) int64 {
	t.Helper()
	run, err := st.Queries.IncrementTaskWorkerRun(context.Background(), sqlcgen.IncrementTaskWorkerRunParams{UpdatedAt: "2026-01-01T00:00:00Z", ID: taskID})
	if err != nil {
		t.Fatalf("IncrementTaskWorkerRun: %v", err)
	}
	return run
}

// TestRedispatchCountsNextWorkerRun: a task whose first spawn was run 1 is
// redispatched as run 2; outbox.resume_dispatched names the run.
func TestRedispatchCountsNextWorkerRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	bumpWorkerRun(t, st, "t1") // handleTask's first spawn
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})
	if claimed, err := d.ClaimAndDispatch(ctx); err != nil || claimed != 1 {
		t.Fatalf("ClaimAndDispatch = (%d, %v), want 1 row", claimed, err)
	}
	run, err := st.Queries.GetTaskWorkerRun(ctx, "t1")
	if err != nil || run != 2 {
		t.Fatalf("worker_run after the redispatch = (%d, %v), want 2", run, err)
	}
	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = ?`, EventResumeDispatched).Scan(&payload); err != nil {
		t.Fatalf("read %s: %v", EventResumeDispatched, err)
	}
	var p struct {
		WorkerRun int64 `json:"worker_run"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.WorkerRun != 2 {
		t.Fatalf("%s payload = %s (%v), want worker_run 2", EventResumeDispatched, payload, err)
	}
}

// failingWorkerRunStore is the real store with the run counter failing.
type failingWorkerRunStore struct {
	Store
}

func (failingWorkerRunStore) IncrementTaskWorkerRun(context.Context, sqlcgen.IncrementTaskWorkerRunParams) (int64, error) {
	return 0, errors.New("disk I/O error (injected)")
}

// TestRedispatchWorkerRunFailureSpawnsNoWorker: a redispatch that cannot
// count its worker run spawns nothing (an uncounted run could not tell its
// calls from the earlier run's): the row is acked as unspawned and the task
// stays executing for the counted resume scan.
func TestRedispatchWorkerRunFailureSpawnsNoWorker(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := NewDispatcher(failingWorkerRunStore{Store: st.Queries}, st, m, spawn.Config{Cmd: []string{"python3", "testdata/echo_session_worker.py"}}, nil)
	if _, err := d.ClaimAndDispatch(ctx); err != nil {
		t.Fatalf("ClaimAndDispatch: %v", err)
	}
	if n := countEvents(t, st, EventResumeDispatched); n != 0 {
		t.Fatalf("%s = %d, want 0 (no worker may run without a counted run)", EventResumeDispatched, n)
	}
	if n := countEvents(t, st, EventWorkerRunFailed); n != 1 {
		t.Fatalf("%s = %d, want 1", EventWorkerRunFailed, n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE id = ? AND dispatched_at IS NOT NULL`, row.ID); n != 1 {
		t.Fatalf("resume row delivered = %d, want 1 (acked for the counted resume scan)", n)
	}
	if s := taskStatusOfStore(t, st, "t1"); s != task.StatusExecuting {
		t.Fatalf("task status = %q, want executing", s)
	}
	if run, err := st.Queries.GetTaskWorkerRun(ctx, "t1"); err != nil || run != 0 {
		t.Fatalf("worker_run = (%d, %v), want 0", run, err)
	}
}
