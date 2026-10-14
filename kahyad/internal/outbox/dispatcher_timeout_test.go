package outbox

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// FIX-I-3: a redispatched worker is bounded by the same task_timeout_min
// POST /v1/task applies at first spawn. Before, processResume ran spawn.Run
// on the scheduler tick's context.Background()-derived context with no
// deadline, its lease heartbeat kept renewing the claim, and the inFlight
// guard turned every later outbox tick into a no-op - one hung resumed
// worker stalled ALL redelivery forever.

// pgrepGroupEmpty reports whether no process remains in process group pgid
// (kahyad/internal/halt/executor_test.go's helper, copied - this package
// owns its own test helpers).
func pgrepGroupEmpty(pgid int) bool {
	out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).CombinedOutput()
	return len(strings.TrimSpace(string(out))) == 0
}

// waitForGroupEmpty polls pgrepGroupEmpty for up to 3s before failing.
func waitForGroupEmpty(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !pgrepGroupEmpty(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("process(es) still in group %d 3s after the worker should have been killed", pgid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForLivePID waits for taskID's worker to register in live and returns
// its pid; it also registers a cleanup that SIGKILLs that process group, so
// a failing test never leaks a hung worker.
func waitForLivePID(t *testing.T, live *task.LiveRegistry, taskID string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := live.PID(taskID); ok && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker never registered")
	return 0
}

// hangDispatcher returns a Dispatcher whose worker is testdata/hang_worker.py
// (never sends a terminal line, never exits on its own), wired to live.
func hangDispatcher(st *store.Store, m *task.Machine, live *task.LiveRegistry) *Dispatcher {
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"python3", "testdata/hang_worker.py"}}, live)
	d.SetLeaseDuration(50 * time.Millisecond)
	return d
}

// runClaimAndDispatch runs one ClaimAndDispatch pass in a goroutine and
// returns a channel carrying its claimed count.
func runClaimAndDispatch(d *Dispatcher, ctx context.Context) <-chan int {
	done := make(chan int, 1)
	go func() {
		n, _ := d.ClaimAndDispatch(ctx)
		done <- n
	}()
	return done
}

func TestProcessResumeHonorsTaskTimeout(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	d := hangDispatcher(st, m, live)
	d.SetTaskTimeout(300 * time.Millisecond)

	done := runClaimAndDispatch(d, context.Background())
	pid := waitForLivePID(t, live, "t1")
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("claimed = %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ClaimAndDispatch never returned: the hung redispatched worker was never killed (no per-row deadline)")
	}
	waitForGroupEmpty(t, pid)
}

func TestTimedOutRedispatchRowIsMarkedDelivered(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	d := hangDispatcher(st, m, live)
	d.SetTaskTimeout(300 * time.Millisecond)

	done := runClaimAndDispatch(d, context.Background())
	waitForLivePID(t, live, "t1")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ClaimAndDispatch never returned")
	}

	// G10-E1: the timed-out row is acked - its next attempt comes only from
	// the resume scan's counted (task_max_crash_resumes), deduped path.
	if !rowAcked(t, st, row.ID) {
		t.Error("timed-out redispatch row left unacknowledged: lease expiry would re-claim it outside the counted resume-scan path")
	}
	if n := countEvents(t, st, EventRedispatchTimedOut); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventRedispatchTimedOut, n)
	}
	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = ?`, EventRedispatchTimedOut).Scan(&payload); err != nil {
		t.Fatalf("read %s payload: %v", EventRedispatchTimedOut, err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["task_id"] != "t1" || p["status"] != spawn.StatusTimeout || p["outbox_id"] != float64(row.ID) {
		t.Errorf("payload = %v, want task_id t1, status %q, outbox_id %d", p, spawn.StatusTimeout, row.ID)
	}
	// Fail-closed: a timeout never ends the task 'done'; it stays executing
	// for the resume scan (and its crash-resume cap).
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Errorf("status = %q, want %q", got, task.StatusExecuting)
	}
}

func TestSetTaskTimeoutZeroRestoresDefault(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, spawn.Config{}, nil)
	if d.taskTimeout != defaultTaskTimeout {
		t.Fatalf("NewDispatcher taskTimeout = %v, want %v", d.taskTimeout, defaultTaskTimeout)
	}
	if defaultTaskTimeout != 30*time.Minute {
		t.Errorf("defaultTaskTimeout = %v, want 30m (config task_timeout_min's default)", defaultTaskTimeout)
	}
	d.SetTaskTimeout(5 * time.Second)
	if d.taskTimeout != 5*time.Second {
		t.Errorf("taskTimeout = %v, want 5s", d.taskTimeout)
	}
	for _, v := range []time.Duration{0, -time.Second} {
		d.SetTaskTimeout(v)
		if d.taskTimeout != defaultTaskTimeout {
			t.Errorf("SetTaskTimeout(%v) -> %v, want the default %v", v, d.taskTimeout, defaultTaskTimeout)
		}
	}
}

// TestClaimAndDispatchStopsWhenBaseContextDone (FIX-I-5): once the daemon's
// lifecycle context ended, a tick claims nothing - the row stays due for
// the next boot.
func TestClaimAndDispatchStopsWhenBaseContextDone(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, m, markerWorker(marker))
	base, cancel := context.WithCancel(context.Background())
	d.SetBaseContext(base)
	cancel()

	if n, err := d.ClaimAndDispatch(context.Background()); n != 0 || err != nil {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 0, nil after shutdown began", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	var attempts int64
	if err := st.DB().QueryRow(`SELECT attempts FROM outbox WHERE id = ?`, row.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || rowAcked(t, st, row.ID) {
		t.Errorf("row attempts = %d, acked = %v; want an untouched, unclaimed row", attempts, rowAcked(t, st, row.ID))
	}
}

// TestProcessResumeCancelsWorkerWhenBaseContextDone (FIX-I-5): a
// redispatched worker that is still running when the daemon shuts down is
// killed (the scheduler's tick context never ends on its own), the row is
// acked as an interrupted run and the task stays 'executing'.
func TestProcessResumeCancelsWorkerWhenBaseContextDone(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	d := hangDispatcher(st, m, live) // default 30m per-row deadline
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.SetBaseContext(base)

	done := runClaimAndDispatch(d, context.Background())
	pid := waitForLivePID(t, live, "t1")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ClaimAndDispatch never returned after the base context ended")
	}
	waitForGroupEmpty(t, pid)
	if !rowAcked(t, st, row.ID) {
		t.Error("the interrupted redispatch row must be acked")
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Errorf("status = %q, want %q", got, task.StatusExecuting)
	}
	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = ?`, EventRedispatchTimedOut).Scan(&payload); err != nil {
		t.Fatalf("read %s: %v", EventRedispatchTimedOut, err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	if p["shutdown"] != true {
		t.Errorf("payload = %v, want shutdown: true", p)
	}
}
