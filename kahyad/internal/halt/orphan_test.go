package halt

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// FIX-I-5: a worker spawned by a kahyad that then died (crash, SIGKILL,
// launchd kickstart) keeps running in its own session; the next boot must
// kill it before its resume scan could start a second worker for the same
// session.

func TestKillOrphanedWorkersAtBootKillsRecordedPGIDAndClearsColumn(t *testing.T) {
	st := testStore(t)
	taskID := "orphan-1"
	insertExecutingTask(t, st, taskID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pidCh := make(chan int, 1)
	doneCh := spawnHangWorker(ctx, taskID, "trace-"+taskID, func(p int) { pidCh <- p })
	pid := <-pidCh
	persistPGID(t, st.Queries, taskID, pid)

	// A brand-new executor: "kahyad just restarted".
	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), task.NewLiveRegistry(), nil, nil, st)
	x.SetWorkerIdentity(func(int) bool { return true })
	killed, err := x.KillOrphanedWorkersAtBoot(context.Background())
	if err != nil || killed != 1 {
		t.Fatalf("KillOrphanedWorkersAtBoot() = %d, %v; want 1, nil", killed, err)
	}
	waitForGroupEmpty(t, pid)
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the orphan's spawn.Run never returned")
	}
	row, err := st.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.WorkerPgid.Valid {
		t.Errorf("worker_pgid = %d, want NULL", row.WorkerPgid.Int64)
	}
	if row.Status != task.StatusExecuting {
		t.Errorf("status = %q, want executing (the resume scan decides next)", row.Status)
	}
	if n := countEventsByKind(t, st.DB(), EventTaskOrphanWorkerKilled); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventTaskOrphanWorkerKilled, n)
	}
	var traceID string
	if err := st.DB().QueryRow(`SELECT trace_id FROM events WHERE kind = ?`, EventTaskOrphanWorkerKilled).Scan(&traceID); err != nil {
		t.Fatal(err)
	}
	if traceID != "trace-"+taskID {
		t.Errorf("ledger trace_id = %q, want the task's own %q", traceID, "trace-"+taskID)
	}
}

func TestKillOrphanedWorkersAtBootSkipsUnknownIdentity(t *testing.T) {
	st := testStore(t)
	tk := insertExecutingTask(t, st, "orphan-2")
	foreign := startForeignGroup(t)
	persistPGID(t, st.Queries, tk.ID, foreign)

	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), nil, nil, nil, st)
	x.SetWorkerIdentity(func(int) bool { return false })
	killed, err := x.KillOrphanedWorkersAtBoot(context.Background())
	if err != nil || killed != 0 {
		t.Fatalf("KillOrphanedWorkersAtBoot() = %d, %v; want 0, nil", killed, err)
	}
	time.Sleep(100 * time.Millisecond)
	if pgrepGroupEmpty(foreign) {
		t.Fatal("a process group that is not our worker was killed")
	}
	row, err := st.Queries.GetTaskByID(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.WorkerPgid.Valid {
		t.Error("the stale record must be cleared")
	}
	if n := countEventsByKind(t, st.DB(), EventTaskOrphanWorkerKilled); n != 0 {
		t.Errorf("%s rows = %d, want 0", EventTaskOrphanWorkerKilled, n)
	}
}

// TestKillOrphanedWorkersAtBootRefusesWithoutIdentity: fail-closed - with
// no identity check wired the sweep kills nothing and keeps the records.
func TestKillOrphanedWorkersAtBootRefusesWithoutIdentity(t *testing.T) {
	st := testStore(t)
	tk := insertExecutingTask(t, st, "orphan-3")
	foreign := startForeignGroup(t)
	persistPGID(t, st.Queries, tk.ID, foreign)

	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), nil, nil, nil, st)
	killed, err := x.KillOrphanedWorkersAtBoot(context.Background())
	if !errors.Is(err, ErrNoWorkerIdentity) || killed != 0 {
		t.Fatalf("KillOrphanedWorkersAtBoot() = %d, %v; want 0, ErrNoWorkerIdentity", killed, err)
	}
	if pgrepGroupEmpty(foreign) {
		t.Fatal("the sweep killed a group without an identity check")
	}
}

// TestKillOrphanedWorkersAtBootWithRealIdentity: the production predicate
// (spawn.NewWorkerIdentity of the configured worker command) recognizes a
// real orphan and kills it.
func TestKillOrphanedWorkersAtBootWithRealIdentity(t *testing.T) {
	st := testStore(t)
	taskID := "orphan-4"
	insertExecutingTask(t, st, taskID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pidCh := make(chan int, 1)
	spawnHangWorker(ctx, taskID, "trace-"+taskID, func(p int) { pidCh <- p })
	pid := <-pidCh
	persistPGID(t, st.Queries, taskID, pid)

	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), nil, nil, nil, st)
	x.SetWorkerIdentity(spawn.NewWorkerIdentity([]string{"python3", "testdata/hang_worker.py"}))
	if killed, err := x.KillOrphanedWorkersAtBoot(context.Background()); err != nil || killed != 1 {
		t.Fatalf("KillOrphanedWorkersAtBoot() = %d, %v; want 1, nil", killed, err)
	}
	waitForGroupEmpty(t, pid)
}
