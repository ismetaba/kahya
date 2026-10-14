package outbox

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// G10-E1 for the two claimed rows the dispatcher can never dispatch at all:
// every row it is done with is acked, so none is re-claimed every lease
// period forever and none locks its task out of the resume scan's counted,
// deduped path (FIX-I-8 counts a claimed-but-unacked row as pending).

// TestUndecodableResumeRowIsAckedSoResumeScanIsNotLockedOut: a resume row
// whose payload cannot be decoded (it still carries its task's back-link)
// is acked with one outbox.payload_invalid row and no spawn; the task's
// next attempt is the scan's counted one. Before, the row stayed claimed,
// the scan's dedupe saw it as pending forever and the task never moved.
func TestUndecodableResumeRowIsAckedSoResumeScanIsNotLockedOut(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	now := task.FixedNanoRFC3339(time.Now())
	bad, err := st.Queries.InsertOutboxRow(ctx, sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-t1", Kind: task.OutboxKindTaskResume, Payload: "{not json",
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
		TaskID: sql.NullString{String: "t1", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, m, markerWorker(marker))
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if !rowAcked(t, st, bad.ID) {
		t.Fatal("undecodable row left unacked: re-claimed every lease forever, and the scan's dedupe counts it as pending")
	}
	if n := countEvents(t, st, "outbox.payload_invalid"); n != 1 {
		t.Errorf("outbox.payload_invalid rows = %d, want 1", n)
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Errorf("status = %q, want %q (nothing ran)", got, task.StatusExecuting)
	}

	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, nil, nil, 3)
	if n, err := r.Scan(ctx); err != nil || n != 1 {
		t.Fatalf("Scan() = %d, %v; want 1, nil", n, err)
	}
	row, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CrashResumes != 1 {
		t.Errorf("crash_resumes = %d, want 1 (the scan's counted enqueue was not locked out)", row.CrashResumes)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL`); n != 1 {
		t.Errorf("pending rows = %d, want exactly the scan's fresh one", n)
	}
}

// TestResumeRowForMissingTaskIsAcked: a resume row whose task row does not
// exist is acked (one outbox.task_load_failed row), never re-claimed.
func TestResumeRowForMissingTaskIsAcked(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	ghost := enqueueResumeRow(t, st, "ghost")
	m := task.NewMachine(st.Queries, st)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, m, markerWorker(marker))
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if !rowAcked(t, st, ghost.ID) {
		t.Fatal("row for a missing task left unacked: lease expiry re-claims it forever")
	}
	if n := countEvents(t, st, "outbox.task_load_failed"); n != 1 {
		t.Errorf("outbox.task_load_failed rows = %d, want 1", n)
	}
}
