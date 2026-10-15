package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// FIX-I-5: http.Server.Shutdown never cancels an active request's context,
// so a worker spawned by handleTask (its own session and process group,
// Setsid) used to outlive kahyad as an orphan; after a launchd restart the
// boot resume scan then started a SECOND worker for the same session.

// pgrepGroupEmpty reports whether no process remains in process group pgid.
func pgrepGroupEmpty(pgid int) bool {
	out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).CombinedOutput()
	return len(strings.TrimSpace(string(out))) == 0
}

func waitForGroupEmpty(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !pgrepGroupEmpty(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("process(es) still in worker group %d 3s after Shutdown", pgid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startHungTask posts one task whose worker (spawn's hang_worker.py: a
// grandchild in its group, never a terminal line, never exits) runs under
// a 30-minute task deadline, and returns its task id and worker pid once
// the worker registered. The POST runs on its own goroutine with a long
// client (never t.Fatalf off the test goroutine; the fixture client's 2s
// timeout would kill the worker by disconnecting first). The worker's
// group is SIGKILLed on cleanup in case the test fails.
func startHungTask(t *testing.T, traceID string) (taskTestFixture, *task.LiveRegistry, string, int) {
	t.Helper()
	live := task.NewLiveRegistry()
	script := filepath.Join(spawnTestdataDir(t), "hang_worker.py")
	f := newTaskTestFixtureWithOptions(t, []string{"python3", script}, 30, "https://upstream.invalid", generousTestGovernor(), func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		srv.SetTaskLiveRegistry(live)
	})
	go func() {
		body, _ := json.Marshal(map[string]any{"prompt": "uzun bir iş", "trace_id": traceID})
		req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kahya-Trace-Id", traceID)
		resp, err := longClient(f.srv.cfg.Socket).Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	var taskID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if taskID == "" {
			_ = f.store.DB().QueryRow(`SELECT id FROM tasks WHERE trace_id = ?`, traceID).Scan(&taskID)
		}
		if taskID != "" {
			if pid, ok := live.PID(taskID); ok && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
				return f, live, taskID, pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker never registered")
	return f, live, "", 0
}

func TestShutdownKillsInFlightWorkerProcessGroup(t *testing.T) {
	traceID := "abcdef0123456789abcdef0123456701"
	f, _, taskID, pid := startHungTask(t, traceID)

	_ = f.srv.Shutdown()
	waitForGroupEmpty(t, pid)

	// Fail-closed: shutdown never ends the task 'done' (nor halts it) - it
	// stays 'executing' for the next boot's resume scan, and the outcome is
	// still recorded (task_timeout) before the DB closes.
	id, status, state := taskRowByTrace(t, f.store, traceID)
	if id != taskID || status != task.StatusExecuting {
		t.Errorf("task %s status = %q, want %q", id, status, task.StatusExecuting)
	}
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, "task_timeout"); n != 1 {
		t.Errorf("task_timeout rows = %d, want 1 (recorded before Shutdown returned)", n)
	}
}

func TestShutdownReturnsBoundedWithHungWorker(t *testing.T) {
	f, _, _, _ := startHungTask(t, "abcdef0123456789abcdef0123456702")
	start := time.Now()
	_ = f.srv.Shutdown()
	// inflightDrainTimeout plus spawn.Run's own bounded return after a
	// group kill; far below the task's 30-minute deadline.
	if d := time.Since(start); d > inflightDrainTimeout+5*time.Second {
		t.Fatalf("Shutdown took %v with a hung worker, want <= %v", d, inflightDrainTimeout+5*time.Second)
	}
	// Idempotent: the fixture's own cleanup calls Shutdown again.
	start = time.Now()
	_ = f.srv.Shutdown()
	if d := time.Since(start); d > time.Second {
		t.Errorf("second Shutdown took %v, want immediate", d)
	}
}

func TestBoundTaskContextCancelsOnLifecycleEnd(t *testing.T) {
	s := New(testConfig(""), testLogger(t), "v-test", healthyDB)

	ctx, cancel := s.boundTaskContext(context.Background(), time.Hour)
	defer cancel()
	s.lifecycleCancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("task context not cancelled when the daemon lifecycle ended")
	}

	// The request context still bounds it (client disconnect kills the
	// worker, as before), and so does the deadline.
	s2 := New(testConfig(""), testLogger(t), "v-test", healthyDB)
	parent, cancelParent := context.WithCancel(context.Background())
	ctx2, cancel2 := s2.boundTaskContext(parent, time.Hour)
	defer cancel2()
	cancelParent()
	if err := waitDone(ctx2); err != nil {
		t.Fatal("task context not cancelled with its request context")
	}
	ctx3, cancel3 := s2.boundTaskContext(context.Background(), 20*time.Millisecond)
	defer cancel3()
	if err := waitDone(ctx3); err != nil {
		t.Fatal("task context ignored its deadline")
	}
	if ctx3.Err() != context.DeadlineExceeded {
		t.Errorf("ctx3.Err() = %v, want DeadlineExceeded", ctx3.Err())
	}
}

func waitDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(2 * time.Second):
		return context.DeadlineExceeded
	}
}

// TestInflightGateRefusesAfterClose: once Shutdown closed the gate, no new
// worker run may begin (a WaitGroup Add racing Wait from zero is misuse).
func TestInflightGateRefusesAfterClose(t *testing.T) {
	var g inflightGate
	if !g.begin() {
		t.Fatal("begin() on an open gate = false")
	}
	done := make(chan bool, 1)
	go func() { done <- g.closeAndWait(time.Second) }()
	time.Sleep(20 * time.Millisecond)
	if g.begin() {
		t.Fatal("begin() after closeAndWait started = true")
	}
	g.end()
	if !<-done {
		t.Fatal("closeAndWait timed out although the one run ended")
	}
	if !g.closeAndWait(time.Second) {
		t.Error("second closeAndWait must return at once")
	}
	var g2 inflightGate
	g2.begin()
	if g2.closeAndWait(20 * time.Millisecond) {
		t.Error("closeAndWait must report a timeout while a run is still in flight")
	}
	g2.end()
}

// TestShutdownTellsClientTaskWillResume (FIX-FINAL shutdown-reported-as-
// timeout): the still-connected `kahya ask` client of a task whose worker
// kahyad's own Shutdown killed was told "Görev zaman aşımına uğradı (30
// dk)." seconds into the run. It now gets the shutdown message naming the
// task, and the task_timeout row carries shutdown:true.
func TestShutdownTellsClientTaskWillResume(t *testing.T) {
	traceID := "abcdef0123456789abcdef0123456702"
	live := task.NewLiveRegistry()
	script := filepath.Join(spawnTestdataDir(t), "hang_worker.py")
	f := newTaskTestFixtureWithOptions(t, []string{"python3", script}, 30, "https://upstream.invalid", generousTestGovernor(), func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		srv.SetTaskLiveRegistry(live)
	})
	bodyCh := make(chan string, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"prompt": "uzun bir iş", "trace_id": traceID})
		req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
		if err != nil {
			bodyCh <- ""
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kahya-Trace-Id", traceID)
		resp, err := longClient(f.srv.cfg.Socket).Do(req)
		if err != nil {
			bodyCh <- ""
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyCh <- string(raw)
	}()
	var taskID string
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		if taskID == "" {
			_ = f.store.DB().QueryRow(`SELECT id FROM tasks WHERE trace_id = ?`, traceID).Scan(&taskID)
		}
		if taskID != "" {
			if p, ok := live.PID(taskID); ok && p > 0 {
				pid = p
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("worker never registered")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	_ = f.srv.Shutdown()
	waitForGroupEmpty(t, pid)
	var sse string
	select {
	case sse = <-bodyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the client's SSE stream never ended")
	}
	if want := fmt.Sprintf(MsgTaskShutdownInterruptedFmt, taskID); !strings.Contains(sse, want) {
		t.Errorf("client SSE = %q, want it to carry %q", sse, want)
	}
	if strings.Contains(sse, fmt.Sprintf(MsgTaskTimeout, 30)) {
		t.Errorf("client SSE still says the task timed out: %q", sse)
	}
	var shutdownFlag int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE trace_id = ? AND kind = 'task_timeout' AND json_extract(payload, '$.shutdown') = 1`, traceID).Scan(&shutdownFlag); err != nil {
		t.Fatal(err)
	}
	if shutdownFlag != 1 {
		t.Errorf("task_timeout rows with shutdown:true = %d, want 1", shutdownFlag)
	}
}
