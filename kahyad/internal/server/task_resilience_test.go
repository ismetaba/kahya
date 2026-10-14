package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/task"
)

// TestTaskWorkerErrorLineMovesStatusToFailed (FIX-I-2): a worker that ITSELF
// reports a terminal error line (model loop ended in error, envelope
// invalid, max turns) has nothing left to resume - the task is 'failed',
// not 'executing' for an endless resume loop.
func TestTaskWorkerErrorLineMovesStatusToFailed(t *testing.T) {
	f := newTaskTestFixtureWithOptions(t, []string{"python3", filepath.Join("testdata", "error_line_worker.py")}, 5, "https://upstream.invalid", generousTestGovernor(), wireDurability)

	traceID := "trace-i2-errline-0000000000000001"
	resp := postTask(t, f.client, traceID, "bir soru")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ev, msg := lastSSEMessage(t, readAllSSE(t, resp))
	if ev != "error" || msg != "model hata" {
		t.Errorf("last SSE = %s %q, want error \"model hata\"", ev, msg)
	}
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusFailed {
		t.Errorf("tasks.status = %q, want %q", status, task.StatusFailed)
	}
	if state != "error" {
		t.Errorf("tasks.state = %q, want error", state)
	}
}

// TestTaskExitWithoutTerminalLineStaysExecuting: an exit without any
// terminal line (crash, kill -9) keeps the W4-07 behavior - 'executing',
// for the resume scan (now bounded by task_max_crash_resumes).
func TestTaskExitWithoutTerminalLineStaysExecuting(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "exit3_worker.py")
	f := newTaskTestFixtureWithOptions(t, []string{"python3", script}, 5, "https://upstream.invalid", generousTestGovernor(), wireDurability)

	traceID := "trace-i2-noterm-00000000000000001"
	resp := postTask(t, f.client, traceID, "bir soru")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev, _ := lastSSEMessage(t, readAllSSE(t, resp)); ev != "error" {
		t.Errorf("last SSE event = %q, want error", ev)
	}
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusExecuting {
		t.Errorf("tasks.status = %q, want %q", status, task.StatusExecuting)
	}
	if state != "error" {
		t.Errorf("tasks.state = %q, want error", state)
	}
}

// TestBudgetBlockPausesTaskAsBlockedUserOnce (FIX-I-2, HANDOFF S4 "tavanda
// gorev DURAKLAR + Telegram bildirimi"; owner decision G10-1: blocked_user,
// resumed by `kahya task resolve <id> --retry`, never an auto-park): the
// governor refuses the worker's first call (token ceiling 1). The task must
// be blocked_user, tasks.state paused_budget must survive the post-spawn
// write, the SSE names the resolve command, the alarm/ledger fire once, and
// the resume scan never picks the task up.
func TestBudgetBlockPausesTaskAsBlockedUserOnce(t *testing.T) {
	upstream := newRecordingUpstream(t) // must never be called
	governor := anthproxy.NewGovernor(anthproxy.Limits{
		TaskTokenCeiling: 1, DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	f := newTaskTestFixtureWithOptions(t, modelCloudWorkerScript(t), 5, upstream.srv.URL, governor, wireDurability)

	traceID := "trace-i2-budget-00000000000000001"
	resp := postTask(t, f.client, traceID, "çok uzun bir görev")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	taskID, status, state := taskRowByTrace(t, f.store, traceID)
	ev, msg := lastSSEMessage(t, frames)
	if want := fmt.Sprintf(MsgTaskPausedBudgetFmt, traceID, taskID); ev != "error" || msg != want {
		t.Errorf("last SSE = %s %q, want error %q", ev, msg, want)
	}
	if status != task.StatusBlockedUser {
		t.Errorf("tasks.status = %q, want %q", status, task.StatusBlockedUser)
	}
	if state != "paused_budget" {
		t.Errorf("tasks.state = %q, want paused_budget (never overwritten by the post-spawn write)", state)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, anthproxy.EventTaskPausedBudget); n != 1 {
		t.Errorf("%s ledger rows = %d, want 1", anthproxy.EventTaskPausedBudget, n)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, "task_budget_blocked"); n != 1 {
		t.Errorf("task_budget_blocked rows = %d, want 1 (handleTask's own outcome row)", n)
	}
	if hits := upstream.hits(); hits != 0 {
		t.Errorf("upstream hits = %d, want 0", hits)
	}
	m := task.NewMachine(f.store.Queries, f.store)
	if n, err := task.NewResume(f.store.Queries, f.store.Queries, f.store.Queries, m, nil, nil, 3).Scan(context.Background()); err != nil || n != 0 {
		t.Errorf("resume scan = (%d, %v), want (0, nil): a budget-paused task is never re-dispatched", n, err)
	}
}

// TestTaskPausedBudgetMessageByteExact pins the Turkish SSE text.
func TestTaskPausedBudgetMessageByteExact(t *testing.T) {
	if MsgTaskPausedBudgetFmt != "Görev bütçe tavanı nedeniyle duraklatıldı. Ayrıntı: kahya log --trace %s — devam için: kahya task resolve %s --retry" {
		t.Errorf("MsgTaskPausedBudgetFmt = %q, not byte-exact", MsgTaskPausedBudgetFmt)
	}
}
