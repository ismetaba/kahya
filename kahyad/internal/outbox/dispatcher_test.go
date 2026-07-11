package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// insertTaskWithEnvelope inserts a tasks row carrying a real, valid,
// marshaled spawn.Envelope in tasks.envelope (exactly what POST /v1/task
// persists in production) plus an optional sessionID, and transitions it
// to 'executing' (kahyad/internal/task.Machine).
func insertTaskWithEnvelope(t *testing.T, st *store.Store, id, sessionID string) sqlcgen.Task {
	t.Helper()
	ctx := context.Background()
	env := spawn.Envelope{
		SchemaVersion: spawn.SchemaVersion, TaskID: id, TraceID: "trace-" + id,
		Kind: "chat", Prompt: "merhaba", Model: "claude-sonnet-5",
		MemoryInjection: true, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	envJSON, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal envelope: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	sid := sql.NullString{}
	if sessionID != "" {
		sid = sql.NullString{String: sessionID, Valid: true}
	}
	row, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, SessionID: sid, State: "running", TaintTier: "untrusted",
		Model:     sql.NullString{String: "claude-sonnet-5", Valid: true},
		Envelope:  sql.NullString{String: string(envJSON), Valid: true},
		UpdatedAt: now, CreatedAt: now, Lane: "normal",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(ctx, row.TraceID, id, task.StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	got, err := st.Queries.GetTaskByID(ctx, id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return got
}

func enqueueResumeRow(t *testing.T, st *store.Store, taskID string) sqlcgen.Outbox {
	t.Helper()
	payload, err := json.Marshal(task.OutboxTaskResumePayload{TaskID: taskID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	now := task.FixedNanoRFC3339(time.Now())
	row, err := st.Queries.InsertOutboxRow(context.Background(), sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-" + taskID, Kind: task.OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertOutboxRow: %v", err)
	}
	return row
}

func newTestDispatcher(st *store.Store, m *task.Machine, cmd []string) *Dispatcher {
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: cmd, LogDir: "", Socket: ""}, nil)
	d.SetLeaseDuration(50 * time.Millisecond)
	return d
}

func TestDispatcherResumesWithOriginalSessionIDAndTraceID(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})

	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("ClaimAndDispatch() error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	// The worker echoed the envelope it actually received back as a delta
	// line - kahya/internal/spawn doesn't expose that to us directly here,
	// so instead assert on the SIDE EFFECTS: the task transitioned to
	// done (worker reported ok) and its session_id is unchanged (the
	// worker echoed the SAME session_id back via the "session" line, which
	// OnSession persists).
	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Errorf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if !got.SessionID.Valid || got.SessionID.String != "sess-original-123" {
		t.Errorf("SessionID = %+v, want sess-original-123 (unchanged - resumed, never a new session)", got.SessionID)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox WHERE dispatched_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count delivered outbox rows: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered outbox rows = %d, want 1", n)
	}

	// The envelope this dispatcher actually built and validated must have
	// carried resume:true, the ORIGINAL trace_id, and the stored
	// session_id - verified directly via buildResumeEnvelope (the exact
	// function ClaimAndDispatch used).
	env, err := d.buildResumeEnvelope(got0(t, st, "t1"))
	if err != nil {
		t.Fatalf("buildResumeEnvelope: %v", err)
	}
	if !env.Resume {
		t.Error("env.Resume = false, want true")
	}
	if env.SessionID == nil || *env.SessionID != "sess-original-123" {
		t.Errorf("env.SessionID = %v, want sess-original-123", env.SessionID)
	}
	if env.TraceID != "trace-t1" {
		t.Errorf("env.TraceID = %q, want trace-t1", env.TraceID)
	}
	if env.TaskID != "t1" {
		t.Errorf("env.TaskID = %q, want t1", env.TaskID)
	}
}

func got0(t *testing.T, st *store.Store, id string) sqlcgen.Task {
	t.Helper()
	row, err := st.Queries.GetTaskByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row
}

func TestDispatcherLeavesRowUnacknowledgedOnNonZeroExitWithoutTerminalLine(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/fail_no_terminal_worker.py"})

	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("ClaimAndDispatch() error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	var dispatchedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT dispatched_at FROM outbox LIMIT 1`).Scan(&dispatchedAt); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if dispatchedAt.Valid {
		t.Error("dispatched_at is set, want NULL (row must be left unacknowledged)")
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusExecuting {
		t.Errorf("status = %q, want unchanged %q (no terminal state on a bare non-zero exit)", got.Status, task.StatusExecuting)
	}
}

func TestDispatcherNeverRedeliversBlockedUserTask(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(context.Background(), "trace-t1", "t1", task.StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}
	enqueueResumeRow(t, st, "t1")
	// A worker command that would fail loudly if ever actually invoked -
	// proves the guard short-circuits BEFORE any spawn attempt.
	d := newTestDispatcher(st, m, []string{"python3", "-c", "import sys; sys.exit(1)"})

	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("ClaimAndDispatch() error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	var dispatchedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT dispatched_at FROM outbox LIMIT 1`).Scan(&dispatchedAt); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if !dispatchedAt.Valid {
		t.Error("dispatched_at is NULL, want set (a guarded row must still be marked delivered - never redelivered)")
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusBlockedUser {
		t.Errorf("status = %q, want unchanged %q", got.Status, task.StatusBlockedUser)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventRedeliveryGuarded).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Errorf("outbox.redelivery_guarded events = %d, want 1", n)
	}
}

// TestClaimOutboxRowPreventsDoubleClaim is the spec's explicit
// "dispatcher lease prevents double-claim under two concurrent
// dispatchers" test: two Dispatcher instances race to claim the exact
// same due row; exactly one must ever win.
func TestClaimOutboxRowPreventsDoubleClaim(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")

	m := task.NewMachine(st.Queries, st)
	d1 := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})
	d2 := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})

	var totalClaimed int64
	var wg sync.WaitGroup
	for _, d := range []*Dispatcher{d1, d2} {
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := d.ClaimAndDispatch(context.Background())
			if err != nil {
				t.Errorf("ClaimAndDispatch() error = %v", err)
				return
			}
			atomic.AddInt64(&totalClaimed, int64(n))
		}()
	}
	wg.Wait()

	if totalClaimed != 1 {
		t.Fatalf("total rows claimed across both dispatchers = %d, want exactly 1", totalClaimed)
	}
}

// TestLeaseExpiryAllowsReClaim proves a claimed-but-never-acknowledged
// row (e.g. the dispatcher process itself crashed mid-dispatch) becomes
// re-claimable once its lease expires, with attempts incrementing on
// every successful claim.
func TestLeaseExpiryAllowsReClaim(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")

	leaseUntil := task.FixedNanoRFC3339(time.Now().Add(-1 * time.Minute)) // already expired
	affected, err := st.Queries.ClaimOutboxRow(context.Background(), sqlcgen.ClaimOutboxRowParams{
		LeaseUntil: sql.NullString{String: leaseUntil, Valid: true}, ID: row.ID,
		LeaseUntil_2: sql.NullString{String: task.FixedNanoRFC3339(time.Now()), Valid: true},
	})
	if err != nil {
		t.Fatalf("ClaimOutboxRow (initial): %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}

	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})
	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("ClaimAndDispatch() error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1 (expired lease must be re-claimable)", claimed)
	}

	var attempts int64
	if err := st.DB().QueryRow(`SELECT attempts FROM outbox WHERE id = ?`, row.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one for the initial claim, one for the re-claim)", attempts)
	}
}
