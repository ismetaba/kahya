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
	"kahya/kahyad/internal/store/sqlcgen"
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

// beginWorkerRun counts one more worker spawn of taskID (tasks.worker_run)
// exactly as kahyad's spawn sites do before a worker starts (FIX-FINAL
// receipt-replay-per-run) - a test's stand-in for "the task was resumed in
// a new worker run". Returns the new run.
func beginWorkerRun(t *testing.T, st *store.Store, taskID string) int64 {
	t.Helper()
	run, err := st.Queries.IncrementTaskWorkerRun(context.Background(), sqlcgen.IncrementTaskWorkerRunParams{
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), ID: taskID,
	})
	if err != nil {
		t.Fatalf("IncrementTaskWorkerRun(%s): %v", taskID, err)
	}
	return run
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
// idempotency test: a resumed run's Execute call for the SAME (task_id,
// tool_name, args_hash) triple an earlier run completed must return the
// ALREADY-stored result WITHOUT running the effect again (FIX-FINAL
// receipt-replay-per-run: only a LATER run replays).
func TestReplayReturnsStoredReceiptAndExecutesZeroTimes(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	beginWorkerRun(t, st, "t1")
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

	beginWorkerRun(t, st, "t1") // the task is resumed: run 2
	second, replayed, err := r.Execute(context.Background(), in, tool.effect("SHOULD-NEVER-RUN"))
	if err != nil {
		t.Fatalf("Execute() replay call error = %v", err)
	}
	if !replayed {
		t.Fatal("Execute() replayed = false, want true for the resumed run's identical call")
	}
	var receiptRun, replayedRun int64
	if err := st.DB().QueryRow(`SELECT run, replayed_run FROM tool_calls WHERE task_id = 't1'`).Scan(&receiptRun, &replayedRun); err != nil {
		t.Fatal(err)
	}
	if receiptRun != 1 || replayedRun != 2 {
		t.Fatalf("receipt (run, replayed_run) = (%d, %d), want (1, 2): written by run 1, consumed by run 2", receiptRun, replayedRun)
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

// TestExecuteInTxEffectDBWriteCommitsAtomicallyWithReceipt proves an
// effect that writes to brain.db itself (via the tx ExecuteInTx hands it)
// lands in the SAME transaction as the receipt row - both present after
// ExecuteInTx returns, matching task spec step 3's "in the same
// transaction that commits the tool's DB effects". (Before FIX-I-12 this
// was Execute's contract; Execute now runs external effects with tx nil.)
func TestExecuteInTxEffectDBWriteCommitsAtomicallyWithReceipt(t *testing.T) {
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

	if _, _, err := r.ExecuteInTx(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("x")),
	}, effect); err != nil {
		t.Fatalf("ExecuteInTx() error = %v", err)
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

// TestConcurrentExecuteSameKeyNeverOverlapsWithinOneRun is the BLOCKER 1
// regression test (task durability core hardening), re-scoped by FIX-FINAL
// receipt-replay-per-run: 8 goroutines call Execute() concurrently for the
// IDENTICAL (task_id, tool_name, args_hash) triple inside ONE worker run.
// Before BLOCKER 1 the replay check + seq allocation + intent insert were
// three separate un-transacted statements, so two goroutines' effects could
// run at the SAME time. Inside one live run an identical call is a
// legitimate repeat and executes - so all 8 run - but strictly one after
// the other (the keyed mutex in-process, idx_tool_calls_live_unique's
// one-in-flight-row rule in the DB): never two at once, one receipt row
// each, no replay, no row left mid-flight. Run under -race.
func TestConcurrentExecuteSameKeyNeverOverlapsWithinOneRun(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	beginWorkerRun(t, st, "t1")
	r := newReceipts(st)
	argsHash := HashArgs([]byte("concurrent-args"))

	var invocations, inFlight, maxInFlight int64
	effect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		n := atomic.AddInt64(&invocations, 1)
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			m := atomic.LoadInt64(&maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt64(&maxInFlight, m, cur) {
				break
			}
		}
		// Hold the "critical section" open briefly so a broken guard has
		// every opportunity to let a second goroutine's effect run
		// concurrently with this one.
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return json.Marshal(map[string]int64{"result": n})
	}

	const n = 8
	var wg sync.WaitGroup
	replayedFlags := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, replayed, err := r.Execute(context.Background(), ExecuteInput{
				TaskID: "t1", TraceID: "trace-1", ToolName: "concurrent_tool", Class: ClassW1, ArgsHash: argsHash,
			}, effect)
			replayedFlags[i], errs[i] = replayed, err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Execute()[%d] error = %v", i, err)
		}
		if replayedFlags[i] {
			t.Fatalf("Execute()[%d] replayed inside the run that wrote the receipt", i)
		}
	}
	if got := atomic.LoadInt64(&invocations); got != n {
		t.Fatalf("effect invocations = %d, want %d (each live repeat executes)", got, n)
	}
	if got := atomic.LoadInt64(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent effects = %d, want 1 (two executions of the identical call overlapped)", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != n {
		t.Errorf("receipt-status tool_calls = %d, want %d (one row per executed call)", got, n)
	}
	if got := countToolCalls(t, st, "t1", CallStatusIntent) + countToolCalls(t, st, "t1", CallStatusExecuting); got != 0 {
		t.Errorf("in-flight tool_calls = %d, want 0 (no row left mid-flight)", got)
	}
}

// TestConcurrentExecuteAfterResumeConsumesEarlierReceiptOnce: run 1
// completed the call once; in run 2, 8 concurrent identical calls get that
// one receipt replayed exactly ONCE (each earlier receipt is consumed at most
// once per run) and the other 7 execute - one at a time.
func TestConcurrentExecuteAfterResumeConsumesEarlierReceiptOnce(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	beginWorkerRun(t, st, "t1")
	r := newReceipts(st)
	in := ExecuteInput{TaskID: "t1", TraceID: "trace-1", ToolName: "concurrent_tool", Class: ClassW1, ArgsHash: HashArgs([]byte("resume-concurrent"))}
	tool := &stubTool{}
	first, _, err := r.Execute(context.Background(), in, tool.effect("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	beginWorkerRun(t, st, "t1")

	var invocations, inFlight, maxInFlight int64
	effect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		atomic.AddInt64(&invocations, 1)
		if cur := atomic.AddInt64(&inFlight, 1); cur > atomic.LoadInt64(&maxInFlight) {
			atomic.StoreInt64(&maxInFlight, cur)
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return json.Marshal(map[string]string{"result": "run-2"})
	}
	const n = 8
	var wg sync.WaitGroup
	var replays int64
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, replayed, err := r.Execute(context.Background(), in, effect)
			errs[i] = err
			if replayed {
				atomic.AddInt64(&replays, 1)
				if string(res) != string(first) {
					errs[i] = errors.New("replayed result " + string(res) + " is not run 1's " + string(first))
				}
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Execute()[%d]: %v", i, err)
		}
	}
	if replays != 1 || invocations != n-1 {
		t.Fatalf("replays/executions = %d/%d, want 1/%d (run 1's one receipt answers one call of run 2)", replays, invocations, n-1)
	}
	if maxInFlight != 1 {
		t.Fatalf("max concurrent effects = %d, want 1", maxInFlight)
	}
}

// TestExecuteInTxRollsBackEffectWriteOnReceiptFailure is the BLOCKER 4
// regression test for the in-transaction path (FIX-I-12 moved it from
// Execute to ExecuteInTx): an effect whose returned result is invalid raw
// JSON makes the POST-EFFECT receipt-envelope marshal fail
// (encoding/json validates a json.RawMessage sub-document's bytes when
// marshaling the CONTAINING struct - see receiptEnvelope). The row must
// end at 'failed' (never stranded at 'executing'), the effect's OWN DB
// write (which ran inside the SAME transaction as the would-be receipt)
// must have rolled back with it, and a second, genuinely fresh call for
// the identical key must re-execute cleanly - never replaying a phantom
// receipt, never refusing as "already in flight".
func TestExecuteInTxRollsBackEffectWriteOnReceiptFailure(t *testing.T) {
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

	_, replayed, err := r.ExecuteInTx(ctx, ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stranded_tool", Class: ClassW1, ArgsHash: argsHash,
	}, badEffect)
	if err == nil {
		t.Fatal("ExecuteInTx() error = nil, want a marshal error")
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
	result, replayed, err := r.ExecuteInTx(ctx, ExecuteInput{
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

// TestExecutePostEffectMarshalFailureLeavesRowFailedNotStranded is the
// BLOCKER 4 regression test for Execute (external effect, tx nil since
// FIX-I-12): a post-effect receipt-envelope marshal failure must leave the
// tool_calls row 'failed', never stranded at 'executing', and a fresh
// attempt for the identical key must re-execute cleanly (no phantom
// replay, no "already in flight" refusal).
func TestExecutePostEffectMarshalFailureLeavesRowFailedNotStranded(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	ctx := context.Background()
	argsHash := HashArgs([]byte("marshal-fail-args"))
	in := ExecuteInput{TaskID: "t1", TraceID: "trace-1", ToolName: "stranded_tool", Class: ClassW1, ArgsHash: argsHash}

	_, replayed, err := r.Execute(ctx, in, func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		return json.RawMessage(`{not valid json`), nil
	})
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

	result, replayed, err := r.Execute(ctx, in, func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
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

// TestExecuteInTxReplayAndClassRMatchExecute: the in-transaction entry
// point shares prepare with Execute - a resumed run's call for the same key
// replays the earlier run's stored receipt without running the effect (a
// repeat in the same run executes), and class R is refused.
func TestExecuteInTxReplayAndClassRMatchExecute(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	beginWorkerRun(t, st, "t1")
	r := newReceipts(st)
	tool := &stubTool{}
	in := ExecuteInput{TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("intx-replay"))}

	first, replayed, err := r.ExecuteInTx(context.Background(), in, tool.effect("first"))
	if err != nil || replayed {
		t.Fatalf("first ExecuteInTx = (%s, %v, %v), want a fresh run", first, replayed, err)
	}
	beginWorkerRun(t, st, "t1")
	second, replayed, err := r.ExecuteInTx(context.Background(), in, tool.effect("second"))
	if err != nil {
		t.Fatalf("second ExecuteInTx: %v", err)
	}
	if !replayed || string(second) != string(first) {
		t.Fatalf("second ExecuteInTx = (%s, replayed=%v), want the stored %s replayed", second, replayed, first)
	}
	if tool.Invocations != 1 {
		t.Fatalf("Invocations = %d, want 1", tool.Invocations)
	}
	// Execute shares the lifecycle: run 1's one receipt is consumed for this
	// run, so the next identical call of run 2 executes.
	if _, replayed, err := r.Execute(context.Background(), in, tool.effect("third")); err != nil || replayed {
		t.Fatalf("Execute after the consumed replay = (replayed=%v, %v), want a fresh execution", replayed, err)
	}
	// A third run replays BOTH earlier receipts once each, oldest first.
	beginWorkerRun(t, st, "t1")
	for i, want := range []string{string(first), `{"result":"third"}`} {
		got, replayed, err := r.Execute(context.Background(), in, tool.effect("SHOULD-NOT-RUN"))
		if err != nil || !replayed || string(got) != want {
			t.Fatalf("run 3 call #%d = (%s, replayed=%v, %v), want %s replayed", i+1, got, replayed, err, want)
		}
	}
	if _, _, err := r.ExecuteInTx(context.Background(), ExecuteInput{TaskID: "t1", ToolName: "r_tool", Class: ClassR, ArgsHash: "x"}, tool.effect("r")); !errors.Is(err, ErrReadOnlyClass) {
		t.Fatalf("ExecuteInTx(class R) error = %v, want ErrReadOnlyClass", err)
	}
}

// TestExecuteDoesNotHoldDBConnectionDuringEffect (FIX-I-12): while an
// EXTERNAL effect is in flight, an unrelated kahyad write (here: a ledger
// append - exactly what /policy/check's Engine.Check does) must not wait for
// the effect to finish. brain.db has ONE pooled connection
// (store.Open: SetMaxOpenConns(1)); a transaction held across the effect
// pinned it.
func TestExecuteDoesNotHoldDBConnectionDuringEffect(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	started := make(chan struct{})
	release := make(chan struct{})
	effect := func(ctx context.Context, _ *sql.Tx) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"ok":true}`), nil
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := r.Execute(context.Background(), ExecuteInput{
			TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW2, ArgsHash: HashArgs([]byte("a")),
		}, effect)
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	logErr := st.LogEvent(ctx, "trace-other", "policy_decision", map[string]any{"probe": true})
	close(release)
	if logErr != nil {
		<-done
		t.Fatalf("concurrent ledger write during effect = %v, want nil (the effect must not pin the only DB connection)", logErr)
	}
	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Fatalf("receipt rows = %d, want 1", n)
	}
}

// TestExecutePassesNilTxToExternalEffect pins Execute's contract (FIX-I-12):
// the effect runs outside any transaction; an effect that must write
// brain.db atomically with its receipt uses ExecuteInTx instead.
func TestExecutePassesNilTxToExternalEffect(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	var gotTx *sql.Tx
	called := false
	if _, _, err := r.Execute(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("nil-tx")),
	}, func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		called, gotTx = true, tx
		return json.RawMessage(`{"ok":true}`), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("effect never ran")
	}
	if gotTx != nil {
		t.Fatal("Execute handed the effect a non-nil *sql.Tx: an external effect must run with no transaction (and no pooled connection) checked out")
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Fatalf("receipt rows = %d, want 1", n)
	}
}
