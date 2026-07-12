//go:build acceptance

package w4gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// scenarioADurationMS is the stub tool's own sleep duration for the
// CI-speed variant - long enough to reliably observe tool_calls row
// status='executing' and kill the worker mid-call before the effect
// finishes, short enough to keep the whole acceptance package under 60s
// (HANDOFF §6 W4 gate + tasks/README.md gate rule).
const scenarioADurationMS = 3000

// TestScenarioA_KillResumeNoDoubleExecution is HANDOFF §6 W4's first gate
// clause, CI-speed: a task calling a W2 tool (w2_slow_stub) is SIGKILLed
// mid-tool-call; on resume it completes WITHOUT double execution (verified
// via the outbox/ledger, exactly like the task spec's own acceptance
// criteria).
func TestScenarioA_KillResumeNoDoubleExecution(t *testing.T) {
	workerScript := filepath.Join(fixturesDir(t), "w2_worker.py")
	pythonBin := findPython3(t)

	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")

	d := bootKahyad(t, daemonOpts{
		workerCmd: []string{pythonBin, workerScript},
		extraEnv: []string{
			"KAHYA_W2_STUB_DURATION_MS=" + strconv.Itoa(scenarioADurationMS),
			"KAHYA_W2_STUB_COUNTER_FILE=" + counterFile,
			"KAHYA_W2_STUB_PID_FILE=" + pidFile,
		},
	})

	// W3-02's ONLY promotion path (the real, user-invoked CLI command) -
	// "Approval satisfied via the normal W3-02 flow (pre-approve ... so the
	// run is unattended)", task spec step 2.
	d.promoteToAutoAllow(t, "w2_slow_stub", "W2", "global")

	traceID := newTraceID()
	resp := d.postTask(t, traceID, "w4-07 scenario A probe")
	drainSSEAsync(resp)

	db := d.openDB(t)
	taskID := waitForTaskID(t, db, traceID, 10*time.Second)

	// Wait until the tool_calls row is genuinely 'executing' (task spec
	// step 3) before killing - never kill blind. Polled via a direct DB
	// connection, NOT GET /v1/task/status - see waitForToolCallStatusDB's
	// own doc comment for why the HTTP path cannot observe this reliably
	// while the slow effect is genuinely in flight.
	waitForToolCallStatusDB(t, db, taskID, "w2_slow_stub", "executing", 5*time.Second)

	// Read the worker's own pid file (NOT GET /v1/task/status - same
	// single-connection-blocking reason) so the kill lands while the
	// effect is still genuinely running, not after it has already
	// finished.
	pid := readPIDFile(t, pidFile, 2*time.Second)
	t.Logf("[%s] killing worker pid=%d mid-tool-call (task=%s trace=%s)", time.Now().Format(time.RFC3339Nano), pid, taskID, traceID)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 worker pid %d: %v", pid, err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("worker pid %d still alive 200ms after SIGKILL", pid)
	}
	t.Logf("[%s] confirmed worker pid=%d gone", time.Now().Format(time.RFC3339Nano), pid)

	// Resume: the resume scan (1s tick, overridden) notices the task still
	// 'executing' with no live worker (once the effect has resolved to a
	// receipt or the worker's own crash is otherwise accounted for), and
	// the outbox dispatcher (1s tick) redelivers - a FRESH worker re-issues
	// the SAME w2_slow_stub call, which Receipts.Execute answers from the
	// now-committed receipt (tool.replayed), never re-running the effect.
	final := waitForTaskStatus(t, d, taskID, 30*time.Second, "done", "failed", "blocked_user")
	if final.Status != "done" {
		t.Fatalf("task %s ended in status=%q, want done\n%s", taskID, final.Status, dumpLogs(d.dirs.homeDir))
	}

	// --- Scenario A evidence (task file's own acceptance criteria) ---

	// wc -l counter_file == 1: the side effect happened EXACTLY once.
	lines := waitForFileLineCount(t, counterFile, 2*time.Second)
	if lines != 1 {
		b, _ := os.ReadFile(counterFile)
		t.Fatalf("counter_file line count = %d, want 1 (double execution!) content=%q", lines, string(b))
	}

	// The receipt-count SQL from the task spec, verbatim:
	// SELECT COUNT(*) FROM tool_calls WHERE task_id=? AND tool_name='w2_slow_stub' AND status='receipt';
	var receiptCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM tool_calls WHERE task_id=? AND tool_name='w2_slow_stub' AND status='receipt'`,
		taskID,
	).Scan(&receiptCount); err != nil {
		t.Fatalf("query tool_calls receipt count: %v", err)
	}
	if receiptCount != 1 {
		t.Fatalf("tool_calls receipt count = %d, want 1", receiptCount)
	}

	// A tool.replayed event exists for this exact trace_id.
	if !waitForEvent(t, db, traceID, "tool.replayed", 1*time.Second) {
		t.Fatalf("no tool.replayed ledger event for trace_id=%s", traceID)
	}

	// All events share the task's trace_id, and the sequence correlates
	// spawn -> resume dispatch -> replay -> done under that ONE trace_id
	// (task spec: `kahya log --trace <id>` shows spawn -> SIGKILL gap ->
	// resume -> tool.replayed -> done under one trace_id).
	// task.transition's own KIND is asserted present here (the payload's
	// "to" field is checked at the SQL layer by scenario B's own more
	// targeted assertions instead) - waitForTaskStatus above has ALREADY
	// confirmed the task's final status is done, so it is enough that a
	// transition occurred at all alongside the other named kinds.
	kinds := eventKindsForTrace(t, db, traceID)
	mustContain(t, kinds, "task_spawned")
	mustContain(t, kinds, "outbox.resume_dispatched")
	mustContain(t, kinds, "tool.replayed")
	mustContain(t, kinds, "task.transition")
}

func mustContain(t *testing.T, haystack []string, want string) {
	t.Helper()
	for _, k := range haystack {
		if k == want {
			return
		}
	}
	t.Fatalf("event kinds %v do not contain %q", haystack, want)
}

// findPython3 locates a python3 interpreter - the fixture worker scripts
// use only the standard library (json/os/subprocess/sys/urllib), so the
// system python3 is sufficient; no worker/.venv dependency is needed.
func findPython3(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/usr/bin/python3", "/usr/local/bin/python3", "/opt/homebrew/bin/python3"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	t.Fatal("no python3 interpreter found on PATH")
	return ""
}
