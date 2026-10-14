package server

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestHandleTaskClearsWorkerPGIDOnExit (FIX-I-13): handleTask's OnStart
// persists the worker's pgid (W6-03) so a halt after a daemon restart can
// still kill it; once spawn.Run has returned the worker is gone and the
// column must be cleared - a stale pgid is a future unrelated process
// group once the pid is reused.
func TestHandleTaskClearsWorkerPGIDOnExit(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", filepath.Join("testdata", "session_worker.py")}, 5)
	traceID := "trace-pgid-clear-0000000000000001"
	frames := readAllSSE(t, postTask(t, f.client, traceID, "merhaba"))
	if len(frames) == 0 || frames[len(frames)-1].event != "result" {
		t.Fatalf("last SSE frame = %+v, want a result", frames)
	}
	var pgid sql.NullInt64
	if err := f.store.DB().QueryRow(`SELECT worker_pgid FROM tasks WHERE trace_id = ?`, traceID).Scan(&pgid); err != nil {
		t.Fatalf("query worker_pgid: %v", err)
	}
	if pgid.Valid {
		t.Fatalf("worker_pgid = %d after the worker exited, want NULL", pgid.Int64)
	}
}
