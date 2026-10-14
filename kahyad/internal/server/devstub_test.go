package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// newDevStubFixture wires a real store.Store + a real
// kahyad/internal/task.Receipts into a real kahyad Server - mirrors
// newTaskDurabilityFixture's own convention, but HandleDevStub is called
// directly (no HTTP/MCP transport needed to exercise the receipt
// lifecycle itself).
func newDevStubFixture(t *testing.T) (*Server, func(taskID string)) {
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

	receipts := task.NewReceipts(st.DB(), st.Queries, st)
	srv := New(cfg, testLogger(t), "v-devstub-test", healthyDB)
	srv.SetDevStub(receipts)

	insert := func(taskID string) { insertDurabilityTestTask(t, st, taskID) }
	return srv, insert
}

// The "effect runs exactly once" twin of W4-07 scenario A now goes through
// the generic /v1/mcp path that writes the receipt (FIX-I-4):
// TestDevStubThroughGenericPathRunsEffectExactlyOnce (mcp_receipts_test.go).

// TestHandleDevStubRequiresTaskID guards the fail-closed posture when the
// kahya-mcp bridge never forwarded X-Kahya-Task-Id at all (empty task_id) -
// there is no (task_id, tool_name, args_hash) key to protect without one.
func TestHandleDevStubRequiresTaskID(t *testing.T) {
	srv, _ := newDevStubFixture(t)
	_, err := srv.HandleDevStub("trace-1", "", DevStubArgs{DurationMs: 1, CounterFile: filepath.Join(t.TempDir(), "c.txt")})
	if err == nil {
		t.Fatal("HandleDevStub with empty task_id: error = nil, want a rejection")
	}
}

// TestHandleDevStubUnwiredIsError guards the "unwired dependency" posture:
// a Server that never had SetDevStub called (every production kahyad
// outside KAHYA_ENV=dev) must refuse the call, never panic or silently
// skip the effect.
func TestHandleDevStubUnwiredIsError(t *testing.T) {
	srv := New(config.Config{}, testLogger(t), "v-devstub-unwired-test", healthyDB)
	_, err := srv.HandleDevStub("trace-1", "t1", DevStubArgs{DurationMs: 1, CounterFile: "/tmp/x"})
	if err == nil {
		t.Fatal("HandleDevStub with no SetDevStub call: error = nil, want a rejection")
	}
}

// TestPolicyCheckNotBlockedByInFlightDevStubEffect is I-12 at the server
// level: while a slow w2_slow_stub effect is in flight (inside
// Receipts.ExecuteDetached, as the generic /v1/mcp path runs it - FIX-I-4),
// the worker's POST /policy/check must still answer promptly. Before
// FIX-I-12 Execute held brain.db's ONE pooled connection in a transaction
// for the whole effect, Engine.Check waited on it to ledger its
// policy_decision, and the worker's 5 s UDS budget turned that into a
// spurious fail-closed DENY.
func TestPolicyCheckNotBlockedByInFlightDevStubEffect(t *testing.T) {
	fx := newTaskTestFixture(t, nil, 1)
	fx.srv.SetDevStub(task.NewReceipts(fx.store.DB(), fx.store.Queries, fx.store))
	insertDurabilityTestTask(t, fx.store, "dev-t1")
	// memory_search is class R; L1 makes it an auto-allow (G01 bootstrap
	// shape) so the expected answer is a plain allow.
	seedAutonomyState(t, fx.store, "memory_search", string(policy.ClassR), "global", 1)

	// FIX-I-4: the stub's receipt lifecycle is the generic /v1/mcp path's
	// Receipts.ExecuteDetached around the plain HandleDevStub effect -
	// driven here exactly that way.
	receipts := task.NewReceipts(fx.store.DB(), fx.store.Queries, fx.store)
	args := DevStubArgs{DurationMs: 1500, CounterFile: filepath.Join(t.TempDir(), "c.txt")}
	argBytes, _ := json.Marshal(args)
	stubDone := make(chan error, 1)
	go func() {
		_, _, err := receipts.ExecuteDetached(context.Background(), task.ExecuteInput{
			TaskID: "dev-t1", TraceID: "trace-1", ToolName: devStubToolName, Class: task.ClassW2, ArgsHash: task.HashArgs(argBytes),
		}, func(context.Context) (json.RawMessage, error) {
			out, err := fx.srv.HandleDevStub("trace-1", "dev-t1", args)
			if err != nil {
				return nil, err
			}
			return json.Marshal(out)
		})
		stubDone <- err
	}()
	var stubErr error
	stubFinished := false
	waitStub := func() {
		if !stubFinished {
			stubErr = <-stubDone
			stubFinished = true
		}
	}
	t.Cleanup(waitStub) // never leave the effect running past the fixture's own cleanup
	// Let the effect start (intent + executing rows are short autocommits).
	time.Sleep(100 * time.Millisecond)

	body, _ := json.Marshal(map[string]any{
		"trace_id": "trace-1", "task_id": "dev-t1", "session_id": nil,
		"tool_name": "memory_search", "tool_input": map[string]any{"query": "x"},
	})
	client := *fx.client
	client.Timeout = time.Second
	start := time.Now()
	resp, err := client.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /policy/check while the dev-stub effect is in flight: %v (after %v) - the effect must not pin brain.db's only connection", err, time.Since(start))
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/policy/check = %d %s, want 200", resp.StatusCode, raw)
	}
	var got policyCheckResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if got.Decision != policy.ResultAllow {
		t.Fatalf("/policy/check decision = %q (%s), want allow", got.Decision, raw)
	}
	select {
	case err := <-stubDone:
		stubFinished = true
		t.Fatalf("dev-stub effect finished (err=%v) before /policy/check answered - the probe did not overlap the effect", err)
	default:
	}
	waitStub()
	if stubErr != nil {
		t.Fatalf("HandleDevStub: %v", stubErr)
	}
	if n := countToolCallsByStatus(t, fx.store, "dev-t1", task.CallStatusReceipt); n != 1 {
		t.Fatalf("receipt rows = %d, want 1", n)
	}
}

func countToolCallsByStatus(t *testing.T, st *store.Store, taskID, status string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM tool_calls WHERE task_id = ? AND status = ?`, taskID, status).Scan(&n); err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	return n
}
