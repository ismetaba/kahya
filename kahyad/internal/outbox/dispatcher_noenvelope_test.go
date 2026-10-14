package outbox

import (
	"context"
	"database/sql"
	"testing"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// TestProcessResumeWithoutStoredEnvelopeFailsTaskAndAcksRow (G10-E2, landed
// with FIX-I-14): a task row with no stored envelope (the briefing's - it
// now enters 'executing', so a kahyad death mid-briefing hands it to the
// resume scan) can never be resumed. Before, processResume ledgered
// outbox.envelope_invalid and returned with the row neither delivered nor
// cancelled: lease expiry re-claimed it every two minutes forever and the
// task never left 'executing'. Now the task fails (fail-closed, never
// 'done') and the row is acked: one envelope_invalid row, nothing more.
func TestProcessResumeWithoutStoredEnvelopeFailsTaskAndAcksRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := "2026-01-01T00:00:00Z"
	if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: "b1", TraceID: "trace-b1", State: "running", UpdatedAt: now, CreatedAt: now, Lane: "normal",
	}); err != nil { // envelope NULL (the briefing's shape)
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace-b1", "b1", task.StatusExecuting); err != nil {
		t.Fatal(err)
	}
	row := enqueueResumeRow(t, st, "b1")
	d := newTestDispatcher(st, m, []string{"/nonexistent/worker"})
	if n, _ := d.ClaimAndDispatch(ctx); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}

	got, err := st.Queries.GetTaskByID(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusFailed {
		t.Errorf("status = %q, want failed (nothing can ever be resumed without an envelope)", got.Status)
	}
	var dispatchedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT dispatched_at FROM outbox WHERE id = ?`, row.ID).Scan(&dispatchedAt); err != nil {
		t.Fatal(err)
	}
	if !dispatchedAt.Valid {
		t.Error("row must be acked, not left for lease-expiry re-claim")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'outbox.envelope_invalid'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("outbox.envelope_invalid rows = %d, want 1", n)
	}
}
