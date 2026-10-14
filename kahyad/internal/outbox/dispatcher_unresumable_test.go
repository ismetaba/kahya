package outbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-I-14 review: once local answers and the briefing walk the status
// machine, the outbox dispatcher can meet task rows that must never get a
// worker - a secret-lane row (W3-08: answered on-device only), a task that
// already ended, an envelope-less row that is parked, a session still
// running inside kahyad. These tests pin each of those decisions.

// markerWorker is a worker command that records the envelope it receives in
// path - the file existing at all means a worker was spawned.
func markerWorker(path string) []string {
	return []string{"python3", "-c", "import sys; open(sys.argv[1], 'wb').write(sys.stdin.buffer.read()); print('{\"type\": \"result\", \"status\": \"ok\"}', flush=True)", path}
}

func assertNoWorkerSpawned(t *testing.T, marker string) {
	t.Helper()
	if raw, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a worker was spawned; it received %s", raw)
	}
}

func countEvents(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count events %s: %v", kind, err)
	}
	return n
}

func rowAcked(t *testing.T, st *store.Store, id int64) bool {
	t.Helper()
	var dispatchedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT dispatched_at FROM outbox WHERE id = ?`, id).Scan(&dispatchedAt); err != nil {
		t.Fatalf("query outbox row %d: %v", id, err)
	}
	return dispatchedAt.Valid
}

// insertTaskWithLaneEnvelope inserts an 'executing' task row whose column
// lane is rowLane and whose stored envelope carries envLane.
func insertTaskWithLaneEnvelope(t *testing.T, st *store.Store, m *task.Machine, id, rowLane, envLane string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	env := spawn.Envelope{
		SchemaVersion: spawn.SchemaVersion, TaskID: id, TraceID: "trace-" + id,
		Kind: "chat", Prompt: "kredi kartı ekstresi ekte, özetler misin", Model: router.ModelSonnet,
		MemoryInjection: true, CreatedAt: now, Lane: envLane, Category: "finans",
	}
	raw, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, State: "running",
		Model:     sql.NullString{String: router.ModelSonnet, Valid: true},
		Envelope:  sql.NullString{String: string(raw), Valid: true},
		UpdatedAt: now, CreatedAt: now, Lane: rowLane,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ctx, "trace-"+id, id, task.StatusExecuting); err != nil {
		t.Fatal(err)
	}
}

// TestProcessResumeRefusesSecretLaneTask: a secret-lane task row (a legacy
// row that stored its envelope, a kahyad restart mid local answer, a
// `kahya task resolve --retry` of one) is never handed to a worker - the
// W3-08 first layer holds even when a resume row reaches the dispatcher.
// The task fails (its local answer is gone) and the row is acked with its
// own ledger kind. Before the review fix the dispatcher spawned the worker
// with the stored lane=secret envelope and the secret prompt.
func TestProcessResumeRefusesSecretLaneTask(t *testing.T) {
	for _, tc := range []struct{ name, rowLane, envLane string }{
		{"row_and_envelope_secret", spawn.LaneSecret, spawn.LaneSecret},
		{"envelope_secret_only", spawn.LaneNormal, spawn.LaneSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			m := task.NewMachine(st.Queries, st)
			insertTaskWithLaneEnvelope(t, st, m, "s1", tc.rowLane, tc.envLane)
			row := enqueueResumeRow(t, st, "s1")
			marker := filepath.Join(t.TempDir(), "envelope.json")
			d := newTestDispatcher(st, m, markerWorker(marker))
			if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
				t.Fatalf("ClaimAndDispatch = (%d, %v), want (1, nil)", n, err)
			}

			assertNoWorkerSpawned(t, marker)
			got, err := st.Queries.GetTaskByID(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != task.StatusFailed {
				t.Errorf("status = %q, want failed", got.Status)
			}
			if !rowAcked(t, st, row.ID) {
				t.Error("resume row left unacked: lease expiry would re-claim it forever")
			}
			if n := countEvents(t, st, EventResumeRefusedSecretLane); n != 1 {
				t.Errorf("%s rows = %d, want 1", EventResumeRefusedSecretLane, n)
			}
			if n := countEvents(t, st, EventResumeDispatched); n != 0 {
				t.Errorf("%s rows = %d, want 0", EventResumeDispatched, n)
			}
		})
	}
}

// TestProcessResumeAcksRowOfTerminalTask: a resume row whose task already
// ended (done/failed - e.g. a scan that read the row as 'executing' just
// before its in-process session finished) is acked without a spawn and
// without an illegal done -> executing attempt. Before, the dispatcher
// attempted that transition, ledgered it, and left the row for lease
// re-claim forever.
func TestProcessResumeAcksRowOfTerminalTask(t *testing.T) {
	for _, final := range []string{task.StatusDone, task.StatusFailed} {
		t.Run(final, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			insertTaskWithEnvelope(t, st, "t1", "sess-1")
			m := task.NewMachine(st.Queries, st)
			if err := m.Transition(ctx, "trace-t1", "t1", final); err != nil {
				t.Fatal(err)
			}
			row := enqueueResumeRow(t, st, "t1")
			marker := filepath.Join(t.TempDir(), "envelope.json")
			d := newTestDispatcher(st, m, markerWorker(marker))
			if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
				t.Fatalf("ClaimAndDispatch = (%d, %v), want (1, nil)", n, err)
			}

			assertNoWorkerSpawned(t, marker)
			if !rowAcked(t, st, row.ID) {
				t.Error("resume row of a terminal task left unacked")
			}
			got, _ := st.Queries.GetTaskByID(ctx, "t1")
			if got.Status != final {
				t.Errorf("status = %q, want unchanged %q", got.Status, final)
			}
			if n := countEvents(t, st, "task.illegal_transition"); n != 0 {
				t.Errorf("task.illegal_transition rows = %d, want 0", n)
			}
			if n := countEvents(t, st, EventRedeliveryGuarded); n != 1 {
				t.Errorf("%s rows = %d, want 1", EventRedeliveryGuarded, n)
			}
			// A second pass finds nothing due.
			if n, _ := d.ClaimAndDispatch(ctx); n != 0 {
				t.Errorf("second pass claimed %d, want 0", n)
			}
		})
	}
}

// TestProcessResumeParkedTaskWithoutEnvelopeEndsFailed (G10-E2, parked
// case): an envelope-less task that sits in bekliyor-yeniden-deneme when
// its resume row is claimed still ends 'failed' - through the legal
// bekliyor-yeniden-deneme -> executing -> failed path. Before, the direct
// -> failed transition was illegal: ledgered task.illegal_transition, the
// row acked, and the task parked forever with nothing left to resume it.
func TestProcessResumeParkedTaskWithoutEnvelopeEndsFailed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: "b2", TraceID: "trace-b2", State: "running", UpdatedAt: now, CreatedAt: now, Lane: "normal",
	}); err != nil { // envelope NULL (the briefing's shape)
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	for _, to := range []string{task.StatusExecuting, task.StatusRetryWait} {
		if err := m.Transition(ctx, "trace-b2", "b2", to); err != nil {
			t.Fatalf("Transition(%s): %v", to, err)
		}
	}
	row := enqueueResumeRow(t, st, "b2")
	d := newTestDispatcher(st, m, []string{"/nonexistent/worker"})
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch = (%d, %v), want (1, nil)", n, err)
	}

	got, err := st.Queries.GetTaskByID(ctx, "b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !rowAcked(t, st, row.ID) {
		t.Error("resume row left unacked")
	}
	if n := countEvents(t, st, "task.illegal_transition"); n != 0 {
		t.Errorf("task.illegal_transition rows = %d, want 0", n)
	}
	if n := countEvents(t, st, "outbox.envelope_invalid"); n != 1 {
		t.Errorf("outbox.envelope_invalid rows = %d, want 1", n)
	}
}

// TestProcessResumeSkipsInProcessSession: a resume row claimed while the
// task's session still runs inside kahyad (LiveRegistry.BeginInProcess - a
// local answer, a briefing) is left alone like a live worker's: no spawn,
// no status change, row not acked. Once the session ended its task is
// terminal and the re-claimed row is acked.
func TestProcessResumeSkipsInProcessSession(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "")
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	end := live.BeginInProcess("t1")
	row := enqueueResumeRow(t, st, "t1")
	marker := filepath.Join(t.TempDir(), "envelope.json")
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: markerWorker(marker)}, live)
	d.SetLeaseDuration(20 * time.Millisecond)
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch = (%d, %v), want (1, nil)", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if rowAcked(t, st, row.ID) {
		t.Error("row acked while the in-process session still runs")
	}
	if got, _ := st.Queries.GetTaskByID(ctx, "t1"); got.Status != task.StatusExecuting {
		t.Errorf("status = %q, want executing (untouched)", got.Status)
	}
	if n := countEvents(t, st, EventResumeSkippedLive); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventResumeSkippedLive, n)
	}

	// The session finishes (terminal first, then it ends) and the lease
	// expires: the re-claim acks the row, still without a worker.
	if err := m.Transition(ctx, "trace-t1", "t1", task.StatusDone); err != nil {
		t.Fatal(err)
	}
	end()
	time.Sleep(40 * time.Millisecond)
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("re-claim = (%d, %v), want (1, nil)", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if !rowAcked(t, st, row.ID) {
		t.Error("re-claimed row of the finished session left unacked")
	}
}
