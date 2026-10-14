package outbox

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/spawn"
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

// TestProcessResumeUnsupportedModeFailsTaskAndAcksRow (G10-E2 step 3, the
// I-4 A guard): a task whose stored envelope is a one-shot mode session
// (reader, stt) is never re-spawned with resume:true - there is no
// conversation to continue. The task fails, the row is acked with one
// outbox.resume_unsupported_mode row, and no worker runs. Left 'executing'
// it would loop through the resume scan on every tick.
func TestProcessResumeUnsupportedModeFailsTaskAndAcksRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  spawn.Envelope
	}{
		{"reader", spawn.Envelope{Mode: spawn.ModeReader, Schema: "mail_summary_v1", Model: router.ModelHaiku}},
		{"stt", spawn.Envelope{Mode: spawn.ModeSTT, InputAudioPath: "/tmp/kahya-test/ses.wav", Model: router.ModelSonnet}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Format(time.RFC3339)
			env := tc.env
			env.SchemaVersion, env.TaskID, env.TraceID = spawn.SchemaVersion, "m1", "trace-m1"
			env.Kind, env.Prompt, env.CreatedAt = "chat", "özetle", now
			raw, err := env.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := env.Validate(); err != nil {
				t.Fatalf("fixture envelope invalid: %v", err)
			}
			if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
				ID: "m1", TraceID: "trace-m1", State: "running",
				SessionID: sql.NullString{String: "sess-m1", Valid: true},
				Envelope:  sql.NullString{String: string(raw), Valid: true},
				UpdatedAt: now, CreatedAt: now, Lane: spawn.LaneNormal,
			}); err != nil {
				t.Fatal(err)
			}
			m := task.NewMachine(st.Queries, st)
			if err := m.Transition(ctx, "trace-m1", "m1", task.StatusExecuting); err != nil {
				t.Fatal(err)
			}
			row := enqueueResumeRow(t, st, "m1")
			marker := t.TempDir() + "/spawned"
			d := newTestDispatcher(st, m, markerWorker(marker))
			if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
				t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
			}
			assertNoWorkerSpawned(t, marker)
			if got := taskStatusOfStore(t, st, "m1"); got != task.StatusFailed {
				t.Errorf("status = %q, want failed (a one-shot %s session is never resumed)", got, tc.name)
			}
			if !rowAcked(t, st, row.ID) {
				t.Error("row must be acked, not left for lease-expiry re-claim")
			}
			if n := countEvents(t, st, EventResumeUnsupportedMode); n != 1 {
				t.Errorf("%s rows = %d, want 1", EventResumeUnsupportedMode, n)
			}
		})
	}
}
