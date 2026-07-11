package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"kahya/kahyad/internal/store"
)

// stubTool is a minimal stand-in for a real kahyad-owned side-effectful
// tool (fs_write/shell_docker/... in production): Invocations counts how
// many times its effect actually RAN (not how many times Execute was
// called) - the invariant every replay test below checks is that a
// replay hit never increments this counter at all.
type stubTool struct {
	Invocations int
}

func (s *stubTool) effect(result string) EffectFunc {
	return func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		s.Invocations++
		return json.Marshal(map[string]string{"result": result})
	}
}

func (s *stubTool) failingEffect(errMsg string) EffectFunc {
	return func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		s.Invocations++
		return nil, errors.New(errMsg)
	}
}

func newReceipts(st *store.Store) *Receipts {
	return NewReceipts(st.DB(), st.Queries, st)
}

func countToolCalls(t *testing.T, st *store.Store, taskID, status string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM tool_calls WHERE task_id = ? AND status = ?`, taskID, status,
	).Scan(&n); err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	return n
}

func TestExecuteWritesIntentExecutingReceipt(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}

	result, replayed, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1,
		ArgsHash: HashArgs([]byte("args-a")),
	}, tool.effect("ok"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if replayed {
		t.Fatal("Execute() replayed = true on a first, fresh call")
	}
	if tool.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1", tool.Invocations)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if decoded["result"] != "ok" {
		t.Errorf("result = %v, want {result: ok}", decoded)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Errorf("receipt-status tool_calls = %d, want 1", n)
	}
}

// TestReplayReturnsStoredReceiptAndExecutesZeroTimes is the spec's core
// idempotency test: a second Execute call for the SAME (task_id,
// tool_name, args_hash) triple must return the ALREADY-stored result
// WITHOUT running the effect again.
func TestReplayReturnsStoredReceiptAndExecutesZeroTimes(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}
	in := ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW2,
		ArgsHash: HashArgs([]byte("args-a")),
	}

	first, _, err := r.Execute(context.Background(), in, tool.effect("first-run"))
	if err != nil {
		t.Fatalf("Execute() first call error = %v", err)
	}
	if tool.Invocations != 1 {
		t.Fatalf("Invocations after first call = %d, want 1", tool.Invocations)
	}

	second, replayed, err := r.Execute(context.Background(), in, tool.effect("SHOULD-NEVER-RUN"))
	if err != nil {
		t.Fatalf("Execute() replay call error = %v", err)
	}
	if !replayed {
		t.Fatal("Execute() replayed = false, want true on the second identical call")
	}
	// The stub effect passed to the SECOND call must never have run.
	if tool.Invocations != 1 {
		t.Errorf("Invocations after replay = %d, want still 1 (effect must not re-run)", tool.Invocations)
	}
	if string(second) != string(first) {
		t.Errorf("replayed result = %s, want identical to first result %s", second, first)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Errorf("receipt-status tool_calls after replay = %d, want still 1 (no new row)", n)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventReplayed).Scan(&n); err != nil {
		t.Fatalf("count tool.replayed events: %v", err)
	}
	if n != 1 {
		t.Errorf("tool.replayed ledger events = %d, want 1", n)
	}
}

// TestReplayScopedToArgsHash proves the replay lookup is scoped to the
// EXACT args_hash - a DIFFERENT call (different args) for the same task
// and tool must execute independently, never short-circuited by an
// unrelated receipt.
func TestReplayScopedToArgsHash(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}
	ctx := context.Background()

	if _, _, err := r.Execute(ctx, ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("args-a")),
	}, tool.effect("a")); err != nil {
		t.Fatalf("Execute(a) error = %v", err)
	}
	_, replayed, err := r.Execute(ctx, ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("args-b")),
	}, tool.effect("b"))
	if err != nil {
		t.Fatalf("Execute(b) error = %v", err)
	}
	if replayed {
		t.Error("Execute(b) replayed = true, want false (different args_hash)")
	}
	if tool.Invocations != 2 {
		t.Errorf("Invocations = %d, want 2 (both distinct calls executed)", tool.Invocations)
	}
}

func TestExecuteRejectsClassR(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}

	_, _, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_read", Class: ClassR, ArgsHash: HashArgs([]byte("x")),
	}, tool.effect("x"))
	if !errors.Is(err, ErrReadOnlyClass) {
		t.Fatalf("Execute() error = %v, want ErrReadOnlyClass", err)
	}
	if tool.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (R-class must never even run the effect via Execute)", tool.Invocations)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&n); err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	if n != 0 {
		t.Errorf("tool_calls rows = %d, want 0 (R-class gets no rows at all)", n)
	}
}

func TestExecuteMarksFailedOnEffectError(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}

	_, _, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("args-a")),
	}, tool.failingEffect("disk full"))
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("Execute() error = %v, want the effect's own error", err)
	}
	if n := countToolCalls(t, st, "t1", CallStatusFailed); n != 1 {
		t.Errorf("failed-status tool_calls = %d, want 1", n)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 0 {
		t.Errorf("receipt-status tool_calls = %d, want 0", n)
	}

	// A later, genuinely fresh attempt for the same triple must still be
	// allowed to run (not blocked by the earlier failure) and gets a new
	// row (a new seq), never a replay.
	result, replayed, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("args-a")),
	}, tool.effect("succeeded-on-retry"))
	if err != nil {
		t.Fatalf("Execute() retry error = %v", err)
	}
	if replayed {
		t.Error("Execute() retry replayed = true, want false (prior attempt only failed, no receipt)")
	}
	var decoded map[string]string
	_ = json.Unmarshal(result, &decoded)
	if decoded["result"] != "succeeded-on-retry" {
		t.Errorf("retry result = %v", decoded)
	}
}

// TestEffectDBWriteCommitsAtomicallyWithReceipt proves an effect that
// writes to brain.db itself (via the tx Execute hands it) lands in the
// SAME transaction as the receipt row - both present after Execute
// returns, matching task spec step 3's "in the same transaction that
// commits the tool's DB effects".
func TestEffectDBWriteCommitsAtomicallyWithReceipt(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)

	effect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		now := "2026-01-01T00:00:00Z"
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			"stub_write", "W1", "global", 2, 0, now,
		); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]bool{"ok": true})
	}

	if _, _, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("x")),
	}, effect); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM autonomy_state WHERE tool = 'stub_write'`).Scan(&n); err != nil {
		t.Fatalf("count autonomy_state: %v", err)
	}
	if n != 1 {
		t.Errorf("autonomy_state rows = %d, want 1 (effect's own DB write must have committed)", n)
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != 1 {
		t.Errorf("receipt-status tool_calls = %d, want 1", got)
	}
}
