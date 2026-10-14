package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// FIX-I-14: local (secret-lane and downgrade-routed) tasks never entered the
// W4-02 status machine - handleSecretLaneTask only wrote tasks.state - so
// their rows sat at the column DEFAULT 'intent' forever: `kahya task show`
// said "Durum: intent" for an answered task and every ⌥⎋ "halted" it again.
// Every task row now walks intent -> executing -> done|failed.

// resultTaskID returns task_id from the last "result" SSE frame.
func resultTaskID(t *testing.T, frames []sseFrame) string {
	t.Helper()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].event != "result" {
			continue
		}
		var r struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(frames[i].data), &r); err != nil {
			t.Fatalf("decode result frame: %v", err)
		}
		return r.TaskID
	}
	t.Fatalf("no result frame in %+v", frames)
	return ""
}

func secretLanePre(answerer secretlane.Answerer) func(srv *Server, st *store.Store) {
	return func(srv *Server, st *store.Store) {
		wireDurability(srv, st)
		srv.SetSecretLane(secretlane.NewClassifier(nil), answerer, nil)
	}
}

func TestSecretLaneTaskReachesDoneStatus(t *testing.T) {
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde işlendi.", nil
	})
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, "https://upstream.invalid", generousTestGovernor(), secretLanePre(answerer))

	resp := postTask(t, f.client, "trace-sl-status-0000000000000001", "kredi kartı ekstresi ekte, özetler misin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	taskID := resultTaskID(t, readAllSSE(t, resp))
	row, err := f.store.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if row.Status != task.StatusDone {
		t.Errorf("status = %q, want %q", row.Status, task.StatusDone)
	}
	if row.State != "done" {
		t.Errorf("state = %q, want done", row.State)
	}
}

// TestSecretLaneTaskFailureReachesFailedStatus: the fail-closed local
// answer (memory pressure, never a cloud fallback) is a terminal failure.
func TestSecretLaneTaskFailureReachesFailedStatus(t *testing.T) {
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "", mlx.ErrLocalModelUnavailable
	})
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, "https://upstream.invalid", generousTestGovernor(), secretLanePre(answerer))

	traceID := "trace-sl-status-0000000000000002"
	resp := postTask(t, f.client, traceID, "kredi kartı ekstresi ekte, özetler misin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev, _ := lastSSEMessage(t, readAllSSE(t, resp)); ev != "error" {
		t.Fatalf("last SSE event = %q, want error", ev)
	}
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusFailed {
		t.Errorf("status = %q, want %q", status, task.StatusFailed)
	}
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
}

// TestLocalDowngradeTaskReachesDoneStatus: a normal-lane task routed local
// by the 80% cost-governor downgrade takes the same handleSecretLaneTask
// path and must also end 'done'.
func TestLocalDowngradeTaskReachesDoneStatus(t *testing.T) {
	governor := anthproxy.Boot([]anthproxy.BootEvent{
		{Ts: time.Now().UTC(), Record: anthproxy.ModelCallRecord{TaskID: "seed", Model: "claude-opus-4-8", USD: 8.0}},
	}, anthproxy.Limits{
		DailyBudgetUSD: 10, MonthlyBudgetUSD: 1000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	if !governor.Downgraded() {
		t.Fatal("test setup: governor not downgraded")
	}
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde (indirgeme) işlendi.", nil
	})
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, "https://upstream.invalid", governor, secretLanePre(answerer))

	resp := postTask(t, f.client, "trace-sl-status-0000000000000003", "bugün hava çok güzel, parkta yürüyüş yaptım")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	taskID := resultTaskID(t, readAllSSE(t, resp))
	row, err := f.store.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if row.Lane != secretlane.LaneNormal {
		t.Fatalf("lane = %q, want normal (downgrade route, not secret content)", row.Lane)
	}
	if row.Status != task.StatusDone {
		t.Errorf("status = %q, want %q", row.Status, task.StatusDone)
	}
}
