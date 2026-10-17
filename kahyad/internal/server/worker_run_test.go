package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// FIX-FINAL receipt-replay-per-run: POST /v1/task's spawn is the task's
// worker run 1, counted before the worker exists.

// TestFirstSpawnIsWorkerRunOne: a cloud task's first (and only) worker runs
// as run 1, and task_worker_started names it.
func TestFirstSpawnIsWorkerRunOne(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", filepath.Join("testdata", "session_worker.py")}, 5)
	traceID := "trace-worker-run-0000000000000001"
	frames := readAllSSE(t, postTask(t, f.client, traceID, "merhaba"))
	if len(frames) == 0 || frames[len(frames)-1].event != "result" {
		t.Fatalf("last SSE frame = %+v, want a result", frames)
	}
	var run int64
	if err := f.store.DB().QueryRow(`SELECT worker_run FROM tasks WHERE trace_id = ?`, traceID).Scan(&run); err != nil {
		t.Fatalf("query worker_run: %v", err)
	}
	if run != 1 {
		t.Fatalf("worker_run after the first spawn = %d, want 1", run)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "task_worker_started"); n != 1 {
		t.Fatalf("task_worker_started = %d, want 1", n)
	}
}

// failingWorkerRunTaskStore is the real task store with the run counter
// failing.
type failingWorkerRunTaskStore struct {
	TaskStore
}

func (failingWorkerRunTaskStore) IncrementTaskWorkerRun(context.Context, sqlcgen.IncrementTaskWorkerRunParams) (int64, error) {
	return 0, errors.New("disk I/O error (injected)")
}

// TestFirstSpawnWithoutWorkerRunStartsNoWorker: when the run cannot be
// counted the worker is never started (fail-closed) and the client gets the
// Turkish unexpected-exit error.
func TestFirstSpawnWithoutWorkerRunStartsNoWorker(t *testing.T) {
	f := newTaskTestFixtureWithOptions(t, []string{"python3", filepath.Join("testdata", "session_worker.py")}, 5, "https://upstream.invalid", haltWindowGovernor(), func(srv *Server, st *store.Store) {
		srv.SetTaskStore(failingWorkerRunTaskStore{TaskStore: st.Queries})
	})
	traceID := "trace-worker-run-0000000000000002"
	frames := readAllSSE(t, postTask(t, f.client, traceID, "merhaba"))
	if len(frames) == 0 || frames[len(frames)-1].event != "error" {
		t.Fatalf("last SSE frame = %+v, want an error", frames)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "task_worker_started"); n != 0 {
		t.Fatalf("task_worker_started = %d, want 0 (no worker without a counted run)", n)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "task_worker_run_failed"); n != 1 {
		t.Fatalf("task_worker_run_failed = %d, want 1", n)
	}
}
