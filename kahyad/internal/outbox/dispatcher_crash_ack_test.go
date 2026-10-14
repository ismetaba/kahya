package outbox

import (
	"context"
	"testing"

	"kahya/kahyad/internal/task"
)

// TestCrashedRedispatchRowIsAckedSoResumeScanCountsNextAttempt (G10-E1,
// landed with FIX-I-3): a resumed worker that exits without a terminal line
// used to leave its row unacknowledged, so lease expiry re-claimed it and
// the dispatcher re-spawned the task forever outside the resume scan - the
// only path that counts tasks.crash_resumes (FIX-I-2's cap). Now the row is
// acked and the next attempt is the scan's own, counted one.
func TestCrashedRedispatchRowIsAckedSoResumeScanCountsNextAttempt(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	stale := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/fail_no_terminal_worker.py"}) // exit 1, no terminal line
	if n, _ := d.ClaimAndDispatch(context.Background()); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
	if !rowAcked(t, st, stale.ID) {
		t.Fatal("crashed redispatch row left unacknowledged: lease expiry would re-claim it outside the counted resume-scan path (crash_resumes never increments)")
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Fatalf("status = %q, want %q (a crash never ends the task)", got, task.StatusExecuting)
	}

	// ...and the resume scan's COUNTED path stays open.
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, nil, nil, 3)
	if n, err := r.Scan(context.Background()); err != nil || n != 1 {
		t.Fatalf("Scan() = %d, %v; want 1, nil", n, err)
	}
	row, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CrashResumes != 1 {
		t.Fatalf("crash_resumes = %d, want 1", row.CrashResumes)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL`); n != 1 {
		t.Errorf("pending resume rows = %d, want exactly the scan's fresh one", n)
	}
}

// TestDispatcherFailsTaskOnExplicitWorkerErrorLine (FIX-I-3, mirrors
// FIX-I-2's first-spawn rule in server.decidePostSpawn): a resumed worker
// that reports its OWN terminal {"type":"error"} line has nothing left to
// resume - the task fails and the row is acked, instead of the old
// unacknowledged row that lease expiry re-claimed (re-running the same
// failing session, and its cloud spend, every lease period).
func TestDispatcherFailsTaskOnExplicitWorkerErrorLine(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); print('{"type": "error", "message": "model hata"}', flush=True); sys.exit(1)`})
	if n, _ := d.ClaimAndDispatch(context.Background()); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusFailed {
		t.Errorf("status = %q, want %q (the worker itself reported a terminal error)", got, task.StatusFailed)
	}
	if !rowAcked(t, st, row.ID) {
		t.Error("row must be acked once the task failed")
	}
	if n := countEvents(t, st, "outbox.redispatch_worker_error"); n != 1 {
		t.Errorf("outbox.redispatch_worker_error rows = %d, want 1", n)
	}
	if n := countEvents(t, st, "outbox.redispatch_crashed"); n != 0 {
		t.Errorf("outbox.redispatch_crashed rows = %d, want 0 (an explicit error line is not a crash)", n)
	}
}
