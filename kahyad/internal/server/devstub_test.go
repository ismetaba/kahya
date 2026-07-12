package server

import (
	"os"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
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

// TestHandleDevStubEffectRunsExactlyOnce is the unit-level twin of W4-07
// scenario A's whole point: calling HandleDevStub TWICE for the exact same
// (task_id, tool_name, args) must run the underlying effect (append to
// counter_file) exactly ONCE - the second call is answered entirely from
// the durable receipt (task.Receipts.Execute's idempotent-replay
// guarantee), never re-executing.
func TestHandleDevStubEffectRunsExactlyOnce(t *testing.T) {
	srv, insertTask := newDevStubFixture(t)
	insertTask("dev-t1")
	counterFile := filepath.Join(t.TempDir(), "counter.txt")
	args := DevStubArgs{DurationMs: 5, CounterFile: counterFile}

	out1, err := srv.HandleDevStub("trace-1", "dev-t1", args)
	if err != nil {
		t.Fatalf("first HandleDevStub call: %v", err)
	}
	if out1.CounterFile != counterFile {
		t.Errorf("out1.CounterFile = %q, want %q", out1.CounterFile, counterFile)
	}

	out2, err := srv.HandleDevStub("trace-1", "dev-t1", args)
	if err != nil {
		t.Fatalf("second (replay) HandleDevStub call: %v", err)
	}
	if out2 != out1 {
		t.Errorf("replayed result = %+v, want identical to first result %+v", out2, out1)
	}

	b, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counter file: %v", err)
	}
	if got := string(b); got != "1\n" {
		t.Errorf("counter file content = %q, want exactly one %q line (side effect ran twice)", got, "1\n")
	}
}

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
