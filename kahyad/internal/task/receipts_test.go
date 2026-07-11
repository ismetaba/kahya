package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestConcurrentExecuteSameKeyRunsEffectExactlyOnce is the BLOCKER 1
// regression test (task durability core hardening): 8 goroutines call
// Execute() concurrently for the IDENTICAL (task_id, tool_name,
// args_hash) triple. Before the fix, the replay check + seq allocation +
// intent insert were three separate un-transacted statements, so more
// than one goroutine could pass the replay guard and land at a distinct
// seq, running the real effect more than once. The real effect must now
// run EXACTLY ONCE - exactly one status=receipt tool_calls row - and
// every other call must observe replayed=true against that SAME stored
// result, never invoking the effect itself. Run under -race.
func TestConcurrentExecuteSameKeyRunsEffectExactlyOnce(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	argsHash := HashArgs([]byte("concurrent-args"))

	var invocations int64
	effect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		atomic.AddInt64(&invocations, 1)
		// Hold the "critical section" open briefly so a broken guard has
		// every opportunity to let a second goroutine's effect run
		// concurrently with this one.
		time.Sleep(10 * time.Millisecond)
		return json.Marshal(map[string]string{"result": "ran-once"})
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]json.RawMessage, n)
	replayedFlags := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, replayed, err := r.Execute(context.Background(), ExecuteInput{
				TaskID: "t1", TraceID: "trace-1", ToolName: "concurrent_tool", Class: ClassW1, ArgsHash: argsHash,
			}, effect)
			results[i], replayedFlags[i], errs[i] = res, replayed, err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Execute()[%d] error = %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&invocations); got != 1 {
		t.Fatalf("effect invocations = %d, want exactly 1 (double-execution guard failed)", got)
	}
	replayedCount := 0
	for _, rf := range replayedFlags {
		if rf {
			replayedCount++
		}
	}
	if replayedCount != n-1 {
		t.Fatalf("replayed count = %d, want %d (n-1: every call but the real one)", replayedCount, n-1)
	}
	for i := 1; i < n; i++ {
		if string(results[i]) != string(results[0]) {
			t.Errorf("results[%d] = %s, want identical to results[0] = %s", i, results[i], results[0])
		}
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != 1 {
		t.Errorf("receipt-status tool_calls = %d, want exactly 1", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusIntent); got != 0 {
		t.Errorf("intent-status tool_calls = %d, want 0 (no row left mid-flight)", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusExecuting); got != 0 {
		t.Errorf("executing-status tool_calls = %d, want 0 (no row left mid-flight)", got)
	}
}

// TestExecutePostEffectMarshalFailureLeavesRowFailedNotStranded is the
// BLOCKER 4 regression test: an effect whose returned result is invalid
// raw JSON makes the POST-EFFECT receipt-envelope marshal fail
// (encoding/json validates a json.RawMessage sub-document's bytes when
// marshaling the CONTAINING struct - see receiptEnvelope). Before the
// fix, this path rolled back the transaction but never marked the
// tool_calls row 'failed', stranding it at 'executing' forever (the
// replay guard never engages against it, and no retry is possible either
// - NextToolCallSeq/InsertToolCallIntent would just add ANOTHER row
// alongside the permanently stuck one). The row must now end at 'failed',
// the effect's OWN DB write (which ran inside the SAME transaction as the
// would-be receipt) must have rolled back with it (not stranded either),
// and a second, genuinely fresh Execute for the identical key must
// re-execute cleanly - never replaying a phantom receipt, never refusing
// as "already in flight".
func TestExecutePostEffectMarshalFailureLeavesRowFailedNotStranded(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	ctx := context.Background()
	argsHash := HashArgs([]byte("marshal-fail-args"))

	badEffect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			"stranded_tool", "W1", "global", 1, 0, "2026-01-01T00:00:00Z",
		); err != nil {
			return nil, err
		}
		// Invalid JSON: the effect otherwise succeeded (its own DB write,
		// above, already ran against tx) but the RESULT it hands back is
		// malformed - simulating a post-effect failure that must still
		// roll back cleanly.
		return json.RawMessage(`{not valid json`), nil
	}

	_, replayed, err := r.Execute(ctx, ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stranded_tool", Class: ClassW1, ArgsHash: argsHash,
	}, badEffect)
	if err == nil {
		t.Fatal("Execute() error = nil, want a marshal error")
	}
	if replayed {
		t.Error("Execute() replayed = true, want false")
	}

	if got := countToolCalls(t, st, "t1", CallStatusFailed); got != 1 {
		t.Errorf("failed-status tool_calls = %d, want 1 (must not be stranded at 'executing')", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusExecuting); got != 0 {
		t.Errorf("executing-status tool_calls = %d, want 0 (row must not be stranded)", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != 0 {
		t.Errorf("receipt-status tool_calls = %d, want 0", got)
	}

	var autonomyRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM autonomy_state WHERE tool = 'stranded_tool'`).Scan(&autonomyRows); err != nil {
		t.Fatalf("count autonomy_state: %v", err)
	}
	if autonomyRows != 0 {
		t.Errorf("autonomy_state rows = %d, want 0 (effect's own DB write must have rolled back with the failed tx, not committed with no receipt)", autonomyRows)
	}

	// A second, genuinely fresh attempt for the SAME key must re-execute
	// cleanly.
	result, replayed, err := r.Execute(ctx, ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stranded_tool", Class: ClassW1, ArgsHash: argsHash,
	}, func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"result": "succeeded-on-retry"})
	})
	if err != nil {
		t.Fatalf("Execute() retry error = %v", err)
	}
	if replayed {
		t.Error("Execute() retry replayed = true, want false (the earlier attempt only failed, no receipt)")
	}
	var decoded map[string]string
	if uerr := json.Unmarshal(result, &decoded); uerr != nil || decoded["result"] != "succeeded-on-retry" {
		t.Errorf("retry result = %s, unmarshal err = %v", result, uerr)
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != 1 {
		t.Errorf("receipt-status tool_calls after retry = %d, want 1", got)
	}
}
