package halt

import (
	"context"
	"database/sql"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// startForeignGroup starts `sleep 300` as the leader of its own process
// group - a stand-in for an unrelated process that reused a stale
// worker_pgid number.
func startForeignGroup(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func persistPGID(t *testing.T, q *sqlcgen.Queries, taskID string, pgid int) {
	t.Helper()
	if err := q.SetTaskWorkerPGID(context.Background(), sqlcgen.SetTaskWorkerPGIDParams{
		WorkerPgid: sql.NullInt64{Int64: int64(pgid), Valid: true}, UpdatedAt: "2026-01-01T00:00:00Z", ID: taskID,
	}); err != nil {
		t.Fatalf("SetTaskWorkerPGID: %v", err)
	}
}

// TestHaltTaskSkipsStalePersistedPGIDThatIsNotAWorker (FIX-I-13): a
// worker_pgid read back from the DB (no in-memory entry) may name a process
// group that is no longer our worker - the pid was reused. Halt must never
// SIGKILL it: an identity mismatch is skipped (halt.stale_pgid_skipped) and
// the stale column cleared.
func TestHaltTaskSkipsStalePersistedPGIDThatIsNotAWorker(t *testing.T) {
	st := testStore(t)
	tk := insertExecutingTask(t, st, "t1")
	foreign := startForeignGroup(t)
	persistPGID(t, st.Queries, tk.ID, foreign)

	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), nil, nil, nil, st) // live=nil -> DB fallback
	x.SetWorkerIdentity(func(pgid int) bool { return false })                        // "not our worker"
	if _, err := x.HaltTask(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if pgrepGroupEmpty(foreign) {
		t.Fatal("unrelated process group was killed via a stale worker_pgid")
	}
	row, err := st.Queries.GetTaskByID(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.WorkerPgid.Valid {
		t.Fatal("stale worker_pgid must be cleared")
	}
	if row.Status != task.StatusUserHalted {
		t.Errorf("status = %q, want user_halted (the rest of the halt still runs)", row.Status)
	}
}

// TestHaltTaskKillsPersistedPGIDWhenIdentityMatchesAndClearsIt (FIX-I-13):
// a real orphan (persisted pgid, no in-memory entry) that still looks like
// the configured worker is killed - group and grandchild - and the column
// is cleared.
func TestHaltTaskKillsPersistedPGIDWhenIdentityMatchesAndClearsIt(t *testing.T) {
	st := testStore(t)
	taskID := "halt-pg-identity"
	insertExecutingTask(t, st, taskID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pidCh := make(chan int, 1)
	doneCh := spawnHangWorker(ctx, taskID, "trace-"+taskID, func(p int) { pidCh <- p })
	pid := <-pidCh
	persistPGID(t, st.Queries, taskID, pid)

	x := NewExecutor(st.Queries, task.NewMachine(st.Queries, st), task.NewLiveRegistry(), nil, nil, st) // empty registry: restart
	x.SetWorkerIdentity(spawn.NewWorkerIdentity([]string{"python3", "testdata/hang_worker.py"}))
	if _, err := x.HaltTask(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	waitForGroupEmpty(t, pid)
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn.Run never returned after the group was killed")
	}
	row, err := st.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.WorkerPgid.Valid {
		t.Errorf("worker_pgid = %d after the DB-sourced kill, want NULL", row.WorkerPgid.Int64)
	}
}
