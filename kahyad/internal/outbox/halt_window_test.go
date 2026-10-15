package outbox

import (
	"context"
	"testing"

	"kahya/kahyad/internal/halt"
	"kahya/kahyad/internal/task"
)

// TestHaltDuringRedispatchWindowKillsWorker (FIX-FINAL
// halt-window-worker-still-spawned): a halt landing in processResume's
// window between its ->executing transition and the worker's OnStart
// found no pid, reported the task halted, and the redispatched worker ran
// anyway. The halt now marks the task; the registering OnStart kills the
// worker before its envelope is written, and the task stays user_halted.
func TestHaltDuringRedispatchWindowKillsWorker(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	d := NewDispatcher(st.Queries, st, m, newTestDispatcher(st, m, nil).spawnCfg, live)
	d.spawnCfg.Cmd = []string{"python3", "testdata/echo_session_worker.py"}
	exec := halt.NewExecutor(st.Queries, m, live, nil, nil, st)
	var haltedNow bool
	d.SetAnthproxyOpener(func(ctx context.Context, taskID, traceID string) (string, string, func() error, error) {
		h, err := exec.HaltTask(context.Background(), taskID)
		if err != nil {
			t.Errorf("HaltTask: %v", err)
		}
		haltedNow = h
		return "http://127.0.0.1:1", "k", func() error { return nil }, nil
	})
	if _, err := d.ClaimAndDispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !haltedNow {
		t.Fatal("fixture: the halt did not land")
	}
	var killed, done int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'outbox.worker_killed_halted'`).Scan(&killed); err != nil {
		t.Fatal(err)
	}
	if killed != 1 {
		t.Errorf("outbox.worker_killed_halted = %d, want 1", killed)
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'task.transition' AND json_extract(payload, '$.to') = 'done'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 0 {
		t.Errorf("task.transition to done rows = %d, want 0", done)
	}
	cur, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != task.StatusUserHalted {
		t.Errorf("status = %s, want user_halted", cur.Status)
	}
}
