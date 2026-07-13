package halt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/outbox"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// testStore builds a real temp-file brain.db (store.Open's own production
// path) - the same "real sqlite, not a fake" posture kahyad/internal/
// outbox and kahyad/internal/policy's own test suites already establish,
// since this package's tests exercise real atomic UPDATE guards
// (ConsumePendingApproval, ConsumeApprovalToken, CancelOutboxRowsByTask)
// a Go-level mock would not meaningfully prove.
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

// testPolicy is a small hand-built Policy covering the one W2 tool this
// package's tests need (shell_docker, level 0 - NEVER auto-allowed at
// level 0, so Engine.Check always mints a pending approval for it).
func testPolicy() policy.Policy {
	tools := []policy.ToolRule{
		{Name: "shell_docker", Class: policy.ClassW2, ScopeKey: "global"},
	}
	byName := make(map[string]policy.ToolRule, len(tools))
	for _, tr := range tools {
		byName[tr.Name] = tr
	}
	return policy.Policy{Tools: tools, ToolsByName: byName}
}

// insertExecutingTask inserts a tasks row (with a real, valid, marshaled
// spawn.Envelope in tasks.envelope - exactly what POST /v1/task persists
// in production, so a resume-scan/outbox-dispatch pass against it behaves
// exactly like the real thing) and transitions it to 'executing'.
func insertExecutingTask(t *testing.T, st *store.Store, id string) sqlcgen.Task {
	t.Helper()
	ctx := context.Background()
	traceID := "trace-" + id
	env := spawn.Envelope{
		SchemaVersion: spawn.SchemaVersion, TaskID: id, TraceID: traceID,
		Kind: "chat", Prompt: "merhaba", Model: "claude-sonnet-5",
		MemoryInjection: true, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	envJSON, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal envelope: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: id, TraceID: traceID, State: "running",
		Model:     sql.NullString{String: "claude-sonnet-5", Valid: true},
		Envelope:  sql.NullString{String: string(envJSON), Valid: true},
		UpdatedAt: now, CreatedAt: now, Lane: "normal",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(ctx, traceID, id, task.StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	got, err := st.Queries.GetTaskByID(ctx, id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return got
}

// enqueueResumeRowForTask writes one immediately-due OutboxKindTaskResume
// row for taskID, with outbox.task_id set (W6-03's own back-link column -
// see migrations/0015_halt_semantics.sql's doc comment) so
// CancelOutboxRowsByTask/the ListDueOutboxRows join guard can both find it.
func enqueueResumeRowForTask(t *testing.T, st *store.Store, taskID string) sqlcgen.Outbox {
	t.Helper()
	payload, err := json.Marshal(task.OutboxTaskResumePayload{TaskID: taskID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	now := task.FixedNanoRFC3339(time.Now())
	row, err := st.Queries.InsertOutboxRow(context.Background(), sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-" + taskID, Kind: task.OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
		TaskID: sql.NullString{String: taskID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertOutboxRow: %v", err)
	}
	return row
}

// pgrepGroup reports whether ANY process remains in process-group pgid
// (mirrors kahyad/internal/spawn/spawn_test.go's own TestRunKillsProcessGroupOnTimeout
// polling helper, one package over).
func pgrepGroupEmpty(pgid int) bool {
	out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).CombinedOutput()
	return len(strings.TrimSpace(string(out))) == 0
}

// waitForGroupEmpty polls pgrepGroupEmpty for up to 3s (SIGKILL is
// immediate, but the OS may take a moment to finish tearing down a
// reparented grandchild's process-table entry) before failing the test.
func waitForGroupEmpty(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if pgrepGroupEmpty(pgid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan process(es) still in group %d 3s after halt", pgid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// spawnHangWorker starts kahyad/internal/halt/testdata/hang_worker.py via
// the REAL kahyad/internal/spawn.Run (Setpgid: true - pid == pgid), wired
// through onStart exactly like kahyad/internal/server's handleTask/
// kahyad/internal/outbox's processResume do in production. Run blocks
// until ctx is done or the process is killed from outside (this fixture
// never sends a terminal stdout line and never exits on its own) - callers
// run it in its own goroutine and synchronize via the returned doneCh.
func spawnHangWorker(ctx context.Context, taskID, traceID string, onStart func(pid int)) (doneCh <-chan spawn.Outcome) {
	env := spawn.Envelope{
		SchemaVersion: spawn.SchemaVersion, TaskID: taskID, TraceID: traceID,
		Kind: "chat", Prompt: "merhaba", Model: "claude-sonnet-5",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	cfg := spawn.Config{Cmd: []string{"python3", "testdata/hang_worker.py"}}
	ch := make(chan spawn.Outcome, 1)
	go func() {
		outcome, _ := spawn.Run(ctx, cfg, env, spawn.Callbacks{OnStart: onStart})
		ch <- outcome
	}()
	return ch
}

// ---- Test (a): process-GROUP kill, in-memory pgid. ----

func TestHaltTaskKillsProcessGroupViaInMemoryPGID(t *testing.T) {
	st := testStore(t)
	taskID := "halt-pg-mem"
	insertExecutingTask(t, st, taskID)

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var pid int
	pidReady := make(chan struct{})
	doneCh := spawnHangWorker(ctx, taskID, "trace-"+taskID, func(p int) {
		pid = p
		live.Register(taskID, p)
		close(pidReady)
	})
	<-pidReady
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}

	ex := NewExecutor(st.Queries, machine, live, nil, nil, st)
	haltedNow, err := ex.HaltTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("HaltTask() error = %v", err)
	}
	if !haltedNow {
		t.Fatal("HaltTask() haltedNow = false, want true")
	}

	// Both the worker pid AND its forked "sleep 300" child must be gone -
	// proves process-GROUP kill, not merely the direct child's own pid.
	waitForGroupEmpty(t, pid)

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn.Run never returned after the group was killed")
	}

	got, err := st.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusUserHalted {
		t.Errorf("status = %q, want %q", got.Status, task.StatusUserHalted)
	}
}

// ---- Test (a) variant: process-GROUP kill survives a simulated daemon
// restart (in-memory registry cleared; only the persisted
// tasks.worker_pgid column remains). ----

func TestHaltTaskKillsProcessGroupViaPersistedPGIDAfterSimulatedRestart(t *testing.T) {
	st := testStore(t)
	taskID := "halt-pg-restart"
	insertExecutingTask(t, st, taskID)

	machine := task.NewMachine(st.Queries, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var pid int
	pidReady := make(chan struct{})
	doneCh := spawnHangWorker(ctx, taskID, "trace-"+taskID, func(p int) {
		pid = p
		// Persist worker_pgid exactly as kahyad/internal/server's handleTask
		// OnStart callback does in production - but deliberately do NOT
		// register into any LiveRegistry: the executor below gets a BRAND
		// NEW, empty LiveRegistry, simulating a daemon crash/restart that
		// wiped the in-memory registry (macOS has no PDEATHSIG - the worker
		// itself keeps running as an orphan exactly like this fixture does).
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if err := st.Queries.SetTaskWorkerPGID(context.Background(), sqlcgen.SetTaskWorkerPGIDParams{
			WorkerPgid: sql.NullInt64{Int64: int64(p), Valid: true}, UpdatedAt: now, ID: taskID,
		}); err != nil {
			t.Errorf("SetTaskWorkerPGID: %v", err)
		}
		close(pidReady)
	})
	<-pidReady
	if pid == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}

	// A fresh, EMPTY LiveRegistry - "kahyad just restarted" (task.LiveRegistry's
	// own doc comment: "A kahyad restart always starts with an EMPTY
	// registry").
	freshLive := task.NewLiveRegistry()
	ex := NewExecutor(st.Queries, machine, freshLive, nil, nil, st)

	haltedNow, err := ex.HaltTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("HaltTask() error = %v", err)
	}
	if !haltedNow {
		t.Fatal("HaltTask() haltedNow = false, want true")
	}

	waitForGroupEmpty(t, pid)

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn.Run never returned after the group was killed")
	}
}

// ---- Test (b): terminal-state exclusion (task.status, outbox cancel,
// approval invalidation + token revocation), then a resume scan + outbox
// tick exactly as daemon startup does yields zero spawns/redeliveries. ----

func TestHaltTaskExcludesFromResumeAndOutboxAfterHalt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	taskID := "halt-terminal-1"
	row := insertExecutingTask(t, st, taskID)
	enqueueResumeRowForTask(t, st, taskID)

	engine := policy.NewEngine(testPolicy(), st.Queries, st)
	decision, err := engine.Check(ctx, policy.CheckInput{
		Tool: "shell_docker", TaskID: taskID, TraceID: row.TraceID, ToolInput: []byte("echo hi"),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Result != policy.ResultNeedsApproval {
		t.Fatalf("decision.Result = %q, want %q (level 0, W2 never auto-allows)", decision.Result, policy.ResultNeedsApproval)
	}
	pendingID := decision.PendingApprovalID
	if pendingID == "" {
		t.Fatal("NEEDS_APPROVAL decision carries no PendingApprovalID")
	}

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	ex := NewExecutor(st.Queries, machine, live, engine, nil, st)

	haltedNow, err := ex.HaltTask(ctx, taskID)
	if err != nil {
		t.Fatalf("HaltTask() error = %v", err)
	}
	if !haltedNow {
		t.Fatal("HaltTask() haltedNow = false, want true")
	}

	// tasks.status = user_halted.
	got, err := st.Queries.GetTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusUserHalted {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusUserHalted)
	}
	if !got.HaltedAt.Valid || got.HaltedAt.String == "" {
		t.Error("halted_at was never stamped")
	}

	// outbox row canceled (undelivered).
	var canceledAt sql.NullString
	var dispatchedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT canceled_at, dispatched_at FROM outbox WHERE task_id = ?`, taskID).Scan(&canceledAt, &dispatchedAt); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	if !canceledAt.Valid {
		t.Error("outbox row was never canceled")
	}
	if dispatchedAt.Valid {
		t.Error("outbox row must not be marked delivered by halt - only canceled")
	}

	// pending approval invalidated (consumed).
	pa, err := st.Queries.GetPendingApproval(ctx, pendingID)
	if err != nil {
		t.Fatalf("GetPendingApproval: %v", err)
	}
	if !pa.ConsumedAt.Valid {
		t.Error("pending approval was never invalidated (consumed_at still NULL)")
	}

	// A decide --approve against the SAME id after the halt must DENY
	// (ErrInvalidPendingApproval - the token store's single-use guard: the
	// row is already consumed, so ConsumePendingApproval affects 0 rows).
	if _, err := engine.Approve(ctx, pendingID, "local", ""); !errors.Is(err, policy.ErrInvalidPendingApproval) {
		t.Errorf("Approve() after halt error = %v, want ErrInvalidPendingApproval", err)
	}

	// Ledger: task.user_halted + approval.invalidated, both under the
	// task's OWN trace_id.
	if n := countEventsByTraceAndKind(t, st, row.TraceID, EventTaskUserHalted); n != 1 {
		t.Errorf("%s events for trace %s = %d, want 1", EventTaskUserHalted, row.TraceID, n)
	}
	if n := countEventsByTraceAndKind(t, st, row.TraceID, policy.EventApprovalInvalidated); n != 1 {
		t.Errorf("%s events for trace %s = %d, want 1", policy.EventApprovalInvalidated, row.TraceID, n)
	}

	// Now run the W4-02 resume scan and an outbox tick EXACTLY as daemon
	// startup does - zero worker spawns, zero redeliveries for this task
	// (the §6 kabul clause: "daemon yeniden başlasa bile görev devam
	// ETMİYOR ve retry edilmiyor").
	resume := task.NewResume(st.Queries, st.Queries, st.Queries, machine, nil, live, 3)
	scanned, err := resume.Scan(ctx)
	if err != nil {
		t.Fatalf("resume.Scan: %v", err)
	}
	if scanned != 0 {
		t.Errorf("resume.Scan examined %d tasks, want 0 (user_halted is excluded from ListExecutingTasks)", scanned)
	}

	dispatcher := outbox.NewDispatcher(st.Queries, st, machine, spawn.Config{Cmd: []string{"python3", "/nonexistent-worker.py"}}, live)
	claimed, err := dispatcher.ClaimAndDispatch(ctx)
	if err != nil {
		t.Fatalf("ClaimAndDispatch: %v", err)
	}
	if claimed != 0 {
		t.Errorf("ClaimAndDispatch claimed %d rows, want 0 (canceled + user_halted-excluded)", claimed)
	}
}

func countEventsByTraceAndKind(t *testing.T, st *store.Store, traceID, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE trace_id = ? AND kind = ?`, traceID, kind).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// ---- Test (c): idempotent double-halt. ----

func TestHaltTaskDoubleHaltIsIdempotentNoOp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	taskID := "halt-idempotent-1"
	row := insertExecutingTask(t, st, taskID)

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	engine := policy.NewEngine(testPolicy(), st.Queries, st)
	ex := NewExecutor(st.Queries, machine, live, engine, nil, st)

	haltedNow1, err := ex.HaltTask(ctx, taskID)
	if err != nil {
		t.Fatalf("first HaltTask() error = %v", err)
	}
	if !haltedNow1 {
		t.Fatal("first HaltTask() haltedNow = false, want true")
	}

	haltedNow2, err := ex.HaltTask(ctx, taskID)
	if err != nil {
		t.Fatalf("second HaltTask() error = %v, want nil (idempotent no-op, never an error)", err)
	}
	if haltedNow2 {
		t.Error("second HaltTask() haltedNow = true, want false (already terminal - a no-op)")
	}

	// Exactly ONE task.user_halted ledger row - the second call must not
	// re-ledger.
	if n := countEventsByTraceAndKind(t, st, row.TraceID, EventTaskUserHalted); n != 1 {
		t.Errorf("%s events = %d, want exactly 1 (double-halt must not duplicate)", EventTaskUserHalted, n)
	}

	// Halting a taskID that was never even inserted is ALSO a no-op
	// success - a panicked ⌥⎋ must never error or corrupt state.
	haltedNow3, err := ex.HaltTask(ctx, "no-such-task-id")
	if err != nil {
		t.Fatalf("HaltTask(unknown id) error = %v, want nil", err)
	}
	if haltedNow3 {
		t.Error("HaltTask(unknown id) haltedNow = true, want false")
	}
}

// ---- Test (d): HaltAll iterates every non-terminal task. ----

func TestHaltAllHaltsEveryNonTerminalTask(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertExecutingTask(t, st, "halt-all-1")
	insertExecutingTask(t, st, "halt-all-2")

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	ex := NewExecutor(st.Queries, machine, live, nil, nil, st)

	n, err := ex.HaltAll(ctx)
	if err != nil {
		t.Fatalf("HaltAll() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("HaltAll() = %d, want 2", n)
	}

	for _, id := range []string{"halt-all-1", "halt-all-2"} {
		got, err := st.Queries.GetTaskByID(ctx, id)
		if err != nil {
			t.Fatalf("GetTaskByID(%s): %v", id, err)
		}
		if got.Status != task.StatusUserHalted {
			t.Errorf("task %s status = %q, want %q", id, got.Status, task.StatusUserHalted)
		}
	}

	// A second HaltAll (nothing left running) halts zero.
	n2, err := ex.HaltAll(ctx)
	if err != nil {
		t.Fatalf("second HaltAll() error = %v", err)
	}
	if n2 != 0 {
		t.Errorf("second HaltAll() = %d, want 0 (nothing non-terminal left)", n2)
	}
}
