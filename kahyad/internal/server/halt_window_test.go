package server

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/halt"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-FINAL halt-window-worker-still-spawned: a halt that landed before a
// task's worker registered its pid was reported done (haltedNow, status
// user_halted), yet the worker was still spawned and ran to completion -
// W1 tools and cloud calls included.

// haltOnInsertStore runs after() right after InsertTask: a ⌥⎋ landing
// between handleTask's InsertTask and its intent->executing transition.
type haltOnInsertStore struct {
	TaskStore
	after func(id string)
}

func (h haltOnInsertStore) InsertTask(ctx context.Context, arg sqlcgen.InsertTaskParams) (sqlcgen.Task, error) {
	row, err := h.TaskStore.InsertTask(ctx, arg)
	if err == nil && h.after != nil {
		h.after(arg.ID)
	}
	return row, err
}

func haltWindowGovernor() *anthproxy.Governor {
	return anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
}

// TestHaltBeforeExecutingNeverSpawnsWorker: the halt wins the intent edge;
// handleTask's ->executing transition fails and it now fails closed - no
// proxy, no worker, a 409 with the Turkish halted-before-start answer.
func TestHaltBeforeExecutingNeverSpawnsWorker(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	var haltedNow bool
	f := newTaskTestFixtureWithOptions(t, []string{"python3", script}, 1, "https://upstream.invalid", haltWindowGovernor(), func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		live := task.NewLiveRegistry()
		srv.SetTaskLiveRegistry(live)
		exec := halt.NewExecutor(st.Queries, task.NewMachine(st.Queries, st), live, nil, nil, st)
		srv.SetTaskStore(haltOnInsertStore{TaskStore: st.Queries, after: func(id string) {
			h, err := exec.HaltTask(context.Background(), id)
			if err != nil {
				t.Errorf("HaltTask: %v", err)
			}
			haltedNow = h
		}})
	})
	traceID := "trace-halt-window-000000000000001"
	resp := postTask(t, f.client, traceID, "test sorusu")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !haltedNow {
		t.Fatal("fixture: the halt did not land")
	}
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), MsgTaskHaltedBeforeStart) {
		t.Errorf("response = %d %s, want 409 with %q", resp.StatusCode, body, MsgTaskHaltedBeforeStart)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "task_worker_started"); n != 0 {
		t.Errorf("task_worker_started = %d, want 0 (the halted task's worker must never start)", n)
	}
	if _, status, state := taskRowByTrace(t, f.store, traceID); status != task.StatusUserHalted || state == "done" {
		t.Errorf("task status=%s state=%s, want user_halted and never done", status, state)
	}
}

// TestHaltAfterExecutingBeforeOnStartKillsWorker: the halt lands after the
// ->executing transition but before the worker registered (inside the
// per-task egress-gate factory NewTaskProxy calls). It finds no pid, but
// marks the task; the registering OnStart then kills the worker before
// the envelope is written - no answer reaches the client.
func TestHaltAfterExecutingBeforeOnStartKillsWorker(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	governor := haltWindowGovernor()
	var haltedNow bool
	f := newTaskTestFixtureWithOptions(t, []string{"python3", script}, 1, "https://upstream.invalid", governor, func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		live := task.NewLiveRegistry()
		srv.SetTaskLiveRegistry(live)
		exec := halt.NewExecutor(st.Queries, task.NewMachine(st.Queries, st), live, nil, nil, st)
		srv.SetAnthproxy(governor, nil, anthproxy.NewPassthroughCredentialSource(), func(taskID, traceID string) func(*http.Request) error {
			h, err := exec.HaltTask(context.Background(), taskID)
			if err != nil {
				t.Errorf("HaltTask: %v", err)
			}
			haltedNow = h
			return nil
		})
	})
	traceID := "trace-halt-window-000000000000002"
	resp := postTask(t, f.client, traceID, "test sorusu")
	frames := readAllSSE(t, resp)
	if !haltedNow {
		t.Fatal("fixture: the halt did not land")
	}
	for _, fr := range frames {
		if fr.event == "result" || (fr.event == "delta" && strings.Contains(fr.data, "test sorusu")) {
			t.Errorf("the halted task's worker still answered: %s %s", fr.event, fr.data)
		}
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "task_worker_killed_halted"); n != 1 {
		t.Errorf("task_worker_killed_halted = %d, want 1", n)
	}
	if _, status, state := taskRowByTrace(t, f.store, traceID); status != task.StatusUserHalted || state == "done" {
		t.Errorf("task status=%s state=%s, want user_halted and never done", status, state)
	}
}
