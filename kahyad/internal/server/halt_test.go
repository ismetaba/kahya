// halt_test.go proves POST /halt's thin HTTP surface actually reaches
// kahyad/internal/halt.Executor end to end over a real UDS socket - the
// deep process-group-kill/terminal-state-exclusion/token-revocation
// mechanics themselves are kahyad/internal/halt's own test suite
// (executor_test.go); this file only proves the wire contract (request
// body shapes, response shape, unwired-503) matches what `kahya halt`/
// hammerspoon/kahya.lua's ⌥⎋ binding actually send.
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
	"kahya/kahyad/internal/halt"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// newHaltTestFixture wires a real store.Store + real task.Machine/
// LiveRegistry + a real halt.Executor (approvals/docker left nil - this
// file only needs the process-status half) into a real kahyad Server
// served over a real UDS socket.
// postHalt POSTs to /halt, now a human-only control-socket route (the W3
// self-approval fix): the client dials the control socket and the per-boot
// bearer is attached.
func postHalt(t *testing.T, client *http.Client, secret string, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/halt", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	return client.Do(req)
}

func newHaltTestFixture(t *testing.T) (client *http.Client, secret string, st *store.Store) {
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
	live := task.NewLiveRegistry()
	exec := halt.NewExecutor(st.Queries, m, live, nil, nil, st)

	srv := New(cfg, testLogger(t), "v-halt-test", healthyDB)
	srv.SetHaltExecutor(exec)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return unixHTTPClient(srv.controlSocketPath), srv.controlSecret, st
}

func insertHaltTestTask(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, SessionID: sql.NullString{String: "sess-1", Valid: true},
		State: "running", UpdatedAt: now, CreatedAt: now, Lane: "normal",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	m := task.NewMachine(st.Queries, st)
	if err := m.Transition(context.Background(), "trace-"+id, id, task.StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
}

func TestHandleHaltUnwiredIs503(t *testing.T) {
	cfg := config.Config{
		DBPath: filepath.Join(t.TempDir(), "brain.db"),
		Socket: filepath.Join(shortSocketDir(t), "k.sock"),
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	srv := New(cfg, testLogger(t), "v-halt-unwired-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	defer srv.Shutdown()

	client := unixHTTPClient(srv.controlSocketPath)
	secret := srv.controlSecret
	body, _ := json.Marshal(map[string]bool{"all": true})
	resp, err := postHalt(t, client, secret, body)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (halt executor not wired)", resp.StatusCode)
	}
}

func TestHandleHaltAllHaltsEveryExecutingTask(t *testing.T) {
	client, secret, st := newHaltTestFixture(t)
	insertHaltTestTask(t, st, "t1")
	insertHaltTestTask(t, st, "t2")

	body, _ := json.Marshal(map[string]bool{"all": true})
	resp, err := postHalt(t, client, secret, body)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var hr haltResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if hr.Halted != 2 {
		t.Fatalf("halted = %d, want 2", hr.Halted)
	}

	for _, id := range []string{"t1", "t2"} {
		row, err := st.Queries.GetTaskByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetTaskByID(%s): %v", id, err)
		}
		if row.Status != task.StatusUserHalted {
			t.Errorf("task %s status = %q, want %q", id, row.Status, task.StatusUserHalted)
		}
	}
}

func TestHandleHaltSingleTaskID(t *testing.T) {
	client, secret, st := newHaltTestFixture(t)
	insertHaltTestTask(t, st, "t1")
	insertHaltTestTask(t, st, "t2")

	body, _ := json.Marshal(map[string]string{"task_id": "t1"})
	resp, err := postHalt(t, client, secret, body)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var hr haltResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if hr.Halted != 1 {
		t.Fatalf("halted = %d, want 1", hr.Halted)
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusUserHalted {
		t.Errorf("t1 status = %q, want %q", got.Status, task.StatusUserHalted)
	}
	got2, err := st.Queries.GetTaskByID(context.Background(), "t2")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got2.Status == task.StatusUserHalted {
		t.Error("t2 must NOT have been halted (only t1 was named)")
	}
}

func TestHandleHaltUnknownTaskIDIsZeroHaltedNotAnError(t *testing.T) {
	client, secret, _ := newHaltTestFixture(t)

	body, _ := json.Marshal(map[string]string{"task_id": "does-not-exist"})
	resp, err := postHalt(t, client, secret, body)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown task_id is a documented no-op, never an error)", resp.StatusCode)
	}
	var hr haltResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if hr.Halted != 0 {
		t.Fatalf("halted = %d, want 0", hr.Halted)
	}
}

func TestHandleHaltEmptyBodyIsBadRequest(t *testing.T) {
	client, secret, _ := newHaltTestFixture(t)

	body, _ := json.Marshal(map[string]string{})
	resp, err := postHalt(t, client, secret, body)
	if err != nil {
		t.Fatalf("POST /halt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (neither task_id nor all=true)", resp.StatusCode)
	}
}
