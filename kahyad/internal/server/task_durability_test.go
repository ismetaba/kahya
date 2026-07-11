package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// newTaskDurabilityFixture wires a real store.Store + a real
// kahyad/internal/task.Machine/Resolver into a real kahyad Server served
// over a real UDS socket (mirrors newJobsTestFixture's own convention).
func newTaskDurabilityFixture(t *testing.T) (client *http.Client, st *store.Store, resolver *task.Resolver) {
	t.Helper()
	cfg := config.Config{
		DBPath: filepath.Join(t.TempDir(), "brain.db"),
		Socket: filepath.Join(shortSocketDir(t), "k.sock"),
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	m := task.NewMachine(st.Queries, st)
	resolver = task.NewResolver(st.Queries, st.Queries, m)

	srv := New(cfg, testLogger(t), "v-task-durability-test", healthyDB)
	srv.SetTaskDurability(resolver, st.Queries, nil)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return unixHTTPClient(cfg.Socket), st, resolver
}

func insertDurabilityTestTask(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, SessionID: sql.NullString{String: "sess-1", Valid: true},
		State: "running", TaintTier: "untrusted", UpdatedAt: now, CreatedAt: now, Lane: "normal",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
}

func TestHandleTaskStatusReturnsStatusAndToolCalls(t *testing.T) {
	client, st, _ := newTaskDurabilityFixture(t)
	insertDurabilityTestTask(t, st, "t1")

	resp, err := client.Get("http://kahyad/v1/task/status?id=t1")
	if err != nil {
		t.Fatalf("GET /v1/task/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body taskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != "t1" {
		t.Errorf("ID = %q, want t1", body.ID)
	}
	if body.Status != task.StatusIntent {
		t.Errorf("Status = %q, want %q", body.Status, task.StatusIntent)
	}
	if body.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", body.SessionID)
	}
	if len(body.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want empty", body.ToolCalls)
	}
}

func TestHandleTaskStatusUnknownTaskIs404(t *testing.T) {
	client, _, _ := newTaskDurabilityFixture(t)
	resp, err := client.Get("http://kahyad/v1/task/status?id=does-not-exist")
	if err != nil {
		t.Fatalf("GET /v1/task/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleTaskResolveAbortMovesToFailed(t *testing.T) {
	client, st, _ := newTaskDurabilityFixture(t)
	insertDurabilityTestTask(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(context.Background(), "trace-t1", "t1", task.StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(context.Background(), "trace-t1", "t1", task.StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}

	body, _ := json.Marshal(map[string]string{"task_id": "t1", "action": "abort"})
	resp, err := client.Post("http://kahyad/v1/task/resolve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/task/resolve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rr struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !rr.OK {
		t.Fatal("ok = false, want true")
	}

	row, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if row.Status != task.StatusFailed {
		t.Errorf("Status = %q, want %q", row.Status, task.StatusFailed)
	}
}

func TestHandleTaskResolveIllegalTransitionIs409(t *testing.T) {
	client, st, _ := newTaskDurabilityFixture(t)
	insertDurabilityTestTask(t, st, "t1") // status stays 'intent'

	body, _ := json.Marshal(map[string]string{"task_id": "t1", "action": "abort"})
	resp, err := client.Post("http://kahyad/v1/task/resolve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/task/resolve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (intent->failed is illegal)", resp.StatusCode)
	}
}
