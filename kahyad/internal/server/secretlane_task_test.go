package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/secretlane"
)

// TestSecretLaneTaskAnsweredLocallyNeverSpawnsWorker drives a full
// lane=="secret" task end-to-end: the worker command is a script that
// would fail loudly if ever executed (so a passing test proves it never
// ran), yet the task still completes successfully - answered entirely by
// the fake local Qwen Answerer wired via SetSecretLane. Confirms the SSE
// contract (task spec: `processed_locally: true` result field) and the
// persisted tasks.lane/secret_category row.
func TestSecretLaneTaskAnsweredLocallyNeverSpawnsWorker(t *testing.T) {
	// A worker command that does not exist at all: if handleTask ever
	// tried to spawn it for this task, spawn.Run would fail and the SSE
	// stream would end in "error" instead of the local answer below -
	// this is what actually PROVES the worker path never ran.
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)

	classifier := secretlane.NewClassifier(nil) // deterministic pre-pass only
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde işlendi: " + prompt, nil
	})
	f.srv.SetSecretLane(classifier, answerer, nil)

	traceID := "trace-secretlane-0000000000000001"
	resp := postTask(t, f.client, traceID, "kredi kartı ekstresi ekte, özetler misin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var gotDelta bool
	var result map[string]any
	for _, fr := range frames {
		switch fr.event {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(fr.data), &d); err == nil && d.Text != "" {
				gotDelta = true
				if d.Text != "Yerelde işlendi: kredi kartı ekstresi ekte, özetler misin" {
					t.Errorf("delta text = %q, unexpected", d.Text)
				}
			}
		case "result":
			if err := json.Unmarshal([]byte(fr.data), &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
		case "error":
			t.Fatalf("got an error SSE frame (worker path may have run): %s", fr.data)
		}
	}
	if !gotDelta {
		t.Fatal("no delta frame with the local answer text was received")
	}
	if result == nil {
		t.Fatal("no result frame received")
	}
	if result["status"] != "ok" {
		t.Errorf("result[status] = %v, want ok", result["status"])
	}
	if result["processed_locally"] != true {
		t.Errorf("result[processed_locally] = %v, want true", result["processed_locally"])
	}

	// Confirm the persisted tasks row.
	row, err := f.store.Queries.GetTaskLane(context.Background(), result["task_id"].(string))
	if err != nil {
		t.Fatalf("GetTaskLane: %v", err)
	}
	if row.Lane != secretlane.LaneSecret {
		t.Errorf("persisted lane = %q, want %q", row.Lane, secretlane.LaneSecret)
	}
	if !row.SecretCategory.Valid || row.SecretCategory.String != secretlane.CategoryFinans {
		t.Errorf("persisted secret_category = %+v, want %q", row.SecretCategory, secretlane.CategoryFinans)
	}
}

// TestSecretLaneTaskMemcheckInsufficientFailsClosed proves the fail-closed
// path end-to-end: an Answerer returning mlx.ErrLocalModelUnavailable
// produces the EXACT documented Turkish SSE error message, never a cloud
// fallback (the worker is still never spawned - same nonexistent-binary
// worker cmd as above).
func TestSecretLaneTaskMemcheckInsufficientFailsClosed(t *testing.T) {
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)

	classifier := secretlane.NewClassifier(nil)
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "", mlx.ErrLocalModelUnavailable
	})
	f.srv.SetSecretLane(classifier, answerer, nil)

	traceID := "trace-secretlane-nomemory-000000001"
	resp := postTask(t, f.client, traceID, "tahlil sonuçları ektedir")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := readAllSSE(t, resp)
	var errMsg string
	for _, fr := range frames {
		if fr.event == "error" {
			var e struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(fr.data), &e); err == nil {
				errMsg = e.Message
			}
		}
		if fr.event == "result" {
			t.Fatalf("got a result frame, want error (fail-closed): %s", fr.data)
		}
	}
	want := mlx.MsgNoLocalMemory + " (" + mlx.MsgNoLocalMemoryGuidance + ")"
	if errMsg != want {
		t.Errorf("error message = %q, want %q", errMsg, want)
	}
}

// TestNormalTaskLaneUnaffectedWhenSecretLaneNotWired proves the
// backward-compatible default: a Server that never calls SetSecretLane at
// all classifies every task lane="normal" unconditionally (every
// pre-W3-08 test/deployment's exact original behavior).
func TestNormalTaskLaneUnaffectedWhenSecretLaneNotWired(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-normal-lane-00000000000001"
	resp := postTask(t, f.client, traceID, "bugün hava çok güzel, parkta yürüyüş yaptım")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	var taskID string
	for _, fr := range frames {
		if fr.event == "result" {
			var r struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal([]byte(fr.data), &r); err == nil {
				taskID = r.TaskID
			}
		}
	}
	if taskID == "" {
		t.Fatal("no task_id observed")
	}
	row, err := f.store.Queries.GetTaskLane(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskLane: %v", err)
	}
	if row.Lane != secretlane.LaneNormal {
		t.Errorf("persisted lane = %q, want %q (SetSecretLane was never called)", row.Lane, secretlane.LaneNormal)
	}
}

// TestSecretLaneNoAnswererWiredFailsClosed proves a classifier-only wiring
// (answerer==nil) still fails closed rather than falling through to the
// worker/cloud path.
func TestSecretLaneNoAnswererWiredFailsClosed(t *testing.T) {
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)
	f.srv.SetSecretLane(secretlane.NewClassifier(nil), nil, nil)

	traceID := "trace-secretlane-noanswerer-00001"
	resp := postTask(t, f.client, traceID, "kredi kartı ekstresi")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	sawError := false
	for _, fr := range frames {
		if fr.event == "error" {
			sawError = true
		}
		if fr.event == "result" {
			t.Fatalf("got a result frame, want error (no answerer wired): %s", fr.data)
		}
	}
	if !sawError {
		t.Fatal("no error SSE frame received")
	}
}
