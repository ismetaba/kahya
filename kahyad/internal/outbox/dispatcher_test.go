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
	"kahya/kahyad/internal/taint"
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
		ID: id, TraceID: "trace-" + id, SessionID: sid, State: "running",
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

// TestDispatcherResumeNeverInsertsSessionTaintRow is the W4-03 step-8
// permanent regression test, verbatim: "a resumed unknown session_id does
// NOT get [a clean row]". insertTaskWithEnvelope's session_id
// ("sess-original-123") never went through kahyad/internal/server's own
// OnSession -> persistSessionStarted transactional insert (this fixture
// bypasses the HTTP path entirely) - after a full resume dispatch
// completes successfully through this exact session_id, taint.Get must
// STILL resolve it to TierTainted (fail-closed: no row was ever inserted,
// and this package's own OnSession callback - dispatcher.go, a
// deliberately SEPARATE code path from kahyad/internal/server's - never
// calls kahyad/internal/taint at all, structurally, not merely by
// runtime luck).
func TestDispatcherResumeNeverInsertsSessionTaintRow(t *testing.T) {
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

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q (resume must still have completed)", got.Status, task.StatusDone)
	}

	tr := taint.New(st.Queries, st)
	tier, terr := tr.Get(context.Background(), "sess-original-123")
	if terr != nil {
		t.Fatalf("taint.Get: %v", terr)
	}
	if tier != taint.TierTainted {
		t.Fatalf("tier for resumed session_id = %q, want %q (resume must never insert a clean row)", tier, taint.TierTainted)
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

// countingLiveRegistry wraps a real *task.LiveRegistry and additionally
// counts how many times Register has ever been called. The BLOCKER 2
// regression test below uses this count as ground truth for "how many
// times was a real worker actually spawned" - independent of how many
// ClaimAndDispatch/processResume invocations raced to get there, and
// independent of Unregister timing (which the counting-only approach
// sidesteps entirely).
type countingLiveRegistry struct {
	inner *task.LiveRegistry
	mu    sync.Mutex
	regs  int
}

func newCountingLiveRegistry() *countingLiveRegistry {
	return &countingLiveRegistry{inner: task.NewLiveRegistry()}
}

func (c *countingLiveRegistry) Register(taskID string, pid int) {
	c.mu.Lock()
	c.regs++
	c.mu.Unlock()
	c.inner.Register(taskID, pid)
}
func (c *countingLiveRegistry) Unregister(taskID string)  { c.inner.Unregister(taskID) }
func (c *countingLiveRegistry) IsLive(taskID string) bool { return c.inner.IsLive(taskID) }
func (c *countingLiveRegistry) Registrations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.regs
}

// TestLiveWorkerNeverDoubleSpawnedAcrossConcurrentDispatchers is the
// BLOCKER 2 regression test (task durability core hardening): a claimed
// row whose worker outlives the row's INITIAL lease must never be
// re-claimed AND re-spawned by a second, concurrent ClaimAndDispatch
// pass. Before the fix, processResume never checked LiveRegistry.IsLive
// before re-spawning, and a claimed row's lease was computed once at
// claim time and never renewed while spawn.Run blocked for the worker's
// entire runtime - a task that ran longer than one lease period got
// re-claimed and a SECOND live worker spawned for the same task/session.
//
// This test uses an initial lease (30ms) much shorter than the slow
// worker's own sleep (300ms), so - absent BOTH the lease-renewal
// heartbeat (BLOCKER 2(b)) and the IsLive skip-guard (BLOCKER 2(a)) - the
// row would become re-claimable well before the first worker finishes,
// and drives a SECOND Dispatcher instance (sharing the SAME LiveRegistry,
// exactly as main.go wires its one real Dispatcher/LiveRegistry pair)
// against the row repeatedly for the whole window a real overlapping
// cron-tick dispatcher could plausibly hit it in. Assertion: exactly one
// real worker was ever spawned (countingLiveRegistry.Registrations),
// exactly one outbox.resume_dispatched ledger event exists, and the task
// ends 'done' - never two live workers for the same task at once.
func TestLiveWorkerNeverDoubleSpawnedAcrossConcurrentDispatchers(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")

	live := newCountingLiveRegistry()
	m := task.NewMachine(st.Queries, st)
	slowCmd := []string{"python3", "testdata/slow_session_worker.py", "0.3"}

	d1 := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: slowCmd}, live)
	d1.SetLeaseDuration(30 * time.Millisecond)
	d2 := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: slowCmd}, live)
	d2.SetLeaseDuration(30 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := d1.ClaimAndDispatch(context.Background()); err != nil {
			t.Errorf("d1.ClaimAndDispatch() error = %v", err)
		}
	}()

	// Give d1 a head start to actually claim the row and register itself
	// live before d2 starts repeatedly trying the same row.
	time.Sleep(20 * time.Millisecond)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := d2.ClaimAndDispatch(context.Background()); err != nil {
			t.Errorf("d2.ClaimAndDispatch() error = %v", err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	wg.Wait()

	if got := live.Registrations(); got != 1 {
		t.Fatalf("live worker registrations = %d, want exactly 1 (double-spawn guard failed)", got)
	}
	if n := countRows(t, st, `SELECT count(*) FROM events WHERE kind = ?`, EventResumeDispatched); n != 1 {
		t.Errorf("outbox.resume_dispatched events = %d, want exactly 1", n)
	}
	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Errorf("status = %q, want %q", got.Status, task.StatusDone)
	}
}

// TestClaimAndDispatchOverlapGuardSkipsConcurrentInvocation is BLOCKER
// 2(c)'s own regression test: a SECOND call to ClaimAndDispatch on the
// SAME Dispatcher instance, made while a FIRST call is still blocked
// inside a long-running processResume (its own slow spawn.Run), must
// return immediately (0, nil) without listing or claiming any row -
// proving the in-flight guard actually short-circuits an overlapping
// call instead of letting two claim passes run concurrently against the
// same Dispatcher, which is exactly the shape an overlapping
// outbox_dispatch cron tick would otherwise produce (robfig/cron/v3
// starts a new goroutine per scheduled fire with no built-in
// skip-if-still-running behavior).
func TestClaimAndDispatchOverlapGuardSkipsConcurrentInvocation(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")

	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/slow_session_worker.py", "0.3"})

	firstDone := make(chan int, 1)
	go func() {
		claimed, err := d.ClaimAndDispatch(context.Background())
		if err != nil {
			t.Errorf("first ClaimAndDispatch() error = %v", err)
		}
		firstDone <- claimed
	}()

	// Let the first call actually claim the row and block inside
	// spawn.Run before the second, overlapping call is attempted.
	time.Sleep(30 * time.Millisecond)

	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("second (overlapping) ClaimAndDispatch() error = %v", err)
	}
	if claimed != 0 {
		t.Errorf("overlapping ClaimAndDispatch() claimed = %d, want 0 (must be skipped while the first call is still in flight)", claimed)
	}

	select {
	case got := <-firstDone:
		if got != 1 {
			t.Errorf("first ClaimAndDispatch() claimed = %d, want 1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first ClaimAndDispatch() never returned")
	}
}
