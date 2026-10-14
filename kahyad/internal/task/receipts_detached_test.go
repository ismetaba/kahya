package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// FIX-I-4: ExecuteDetached is the receipt lifecycle the REAL side-effectful
// tools (fs_write/fs_delete/shell_docker/shell_host/applescript_run/jxa_run/
// shortcuts_run and the generic /v1/mcp W-tools) run their effect through,
// and Replay is the lookup they call BEFORE their policy check.

func eventCount(t *testing.T, r *Receipts, kind string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

func detachedInput(taskID, args string) ExecuteInput {
	return ExecuteInput{TaskID: taskID, TraceID: "trace-" + taskID, ToolName: "fs_write", Class: ClassW1, ArgsHash: HashArgs([]byte(args))}
}

// TestExecuteDetachedDoesNotHoldConnectionDuringEffect: the effect itself
// reads brain.db (exactly what a real tool's ledger/log calls do while it
// runs) - with the store's ONE pooled connection free during the effect,
// that read answers at once.
func TestExecuteDetachedDoesNotHoldConnectionDuringEffect(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	_, _, err := r.ExecuteDetached(context.Background(), detachedInput("t1", "a"), func(ctx context.Context) (json.RawMessage, error) {
		qctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if _, err := st.Queries.GetTaskByID(qctx, "t1"); err != nil {
			return nil, err
		}
		if err := st.LogEvent(qctx, "trace-t1", "probe", map[string]any{"in_effect": true}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	if err != nil {
		t.Fatalf("ExecuteDetached: %v (a brain.db read/write inside the effect must not wait on a connection the lifecycle holds)", err)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Fatalf("receipt rows = %d, want 1", n)
	}
}

func TestExecuteDetachedReplaySecondCall(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	runs := 0
	effect := func(context.Context) (json.RawMessage, error) {
		runs++
		return json.Marshal(map[string]int{"run": runs})
	}
	in := detachedInput("t1", "same-bytes")
	first, replayed, err := r.ExecuteDetached(context.Background(), in, effect)
	if err != nil || replayed {
		t.Fatalf("first ExecuteDetached = (%s, replayed=%v, %v), want a fresh run", first, replayed, err)
	}
	second, replayed, err := r.ExecuteDetached(context.Background(), in, effect)
	if err != nil {
		t.Fatalf("second ExecuteDetached: %v", err)
	}
	if !replayed || string(second) != string(first) {
		t.Fatalf("second ExecuteDetached = (%s, replayed=%v), want the stored %s replayed", second, replayed, first)
	}
	if runs != 1 {
		t.Fatalf("effect ran %d times, want 1", runs)
	}
	if n := eventCount(t, r, EventReplayed); n != 1 {
		t.Fatalf("tool.replayed events = %d, want 1", n)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 1 {
		t.Fatalf("receipt rows = %d, want 1", n)
	}
}

// TestReplayBeforeAnyAttemptIsMiss: Replay claims nothing - a miss writes no
// tool_calls row and no ledger row; after a completed attempt it answers the
// stored result and ledgers tool.replayed.
func TestReplayBeforeAnyAttemptIsMiss(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	in := detachedInput("t1", "replay-miss")
	res, hit, err := r.Replay(context.Background(), in)
	if err != nil || hit || res != nil {
		t.Fatalf("Replay before any attempt = (%s, hit=%v, %v), want a clean miss", res, hit, err)
	}
	var rows int
	if err := r.db.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("tool_calls rows after a Replay miss = %d, want 0 (Replay must never claim an intent)", rows)
	}
	if n := eventCount(t, r, EventReplayed); n != 0 {
		t.Fatalf("tool.replayed after a miss = %d, want 0", n)
	}

	stored, _, err := r.ExecuteDetached(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"written":true}`), nil
	})
	if err != nil {
		t.Fatalf("ExecuteDetached: %v", err)
	}
	res, hit, err = r.Replay(context.Background(), in)
	if err != nil || !hit || string(res) != string(stored) {
		t.Fatalf("Replay after a receipt = (%s, hit=%v, %v), want the stored %s", res, hit, err, stored)
	}
	if n := eventCount(t, r, EventReplayed); n != 1 {
		t.Fatalf("tool.replayed after a hit = %d, want 1", n)
	}
	// A receipt for other bytes is not a hit.
	if _, hit, err := r.Replay(context.Background(), detachedInput("t1", "other-bytes")); err != nil || hit {
		t.Fatalf("Replay for other args = (hit=%v, %v), want a miss", hit, err)
	}
}

// failingReceiptStore is the real store with MarkToolCallReceipt failing -
// the receipt write after a completed external effect.
type failingReceiptStore struct {
	ToolCallStore
}

func (failingReceiptStore) MarkToolCallReceipt(context.Context, sqlcgen.MarkToolCallReceiptParams) error {
	return errors.New("disk I/O error (injected)")
}

// TestExecuteDetachedReceiptPersistFailureLeavesRowExecuting: the effect HAS
// run but its receipt cannot be written. The row must stay 'executing' (never
// 'failed': a failed row frees the key, so the very next identical call would
// run the effect a second time), the caller gets ErrReceiptPersistFailed, and
// tool.receipt_persist_failed is ledgered. The resume scan then applies
// HANDOFF §6 W4 to the receipt-less row (W1 auto-retry, W2/W3 never).
func TestExecuteDetachedReceiptPersistFailureLeavesRowExecuting(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := &Receipts{db: st.DB(), q: failingReceiptStore{ToolCallStore: st.Queries}, ledger: st, now: time.Now, locks: newKeyedMutexes()}
	runs := 0
	effect := func(context.Context) (json.RawMessage, error) {
		runs++
		return json.RawMessage(`{"ok":true}`), nil
	}
	in := detachedInput("t1", "persist-fail")
	in.Class = ClassW2
	res, replayed, err := r.ExecuteDetached(context.Background(), in, effect)
	if !errors.Is(err, ErrReceiptPersistFailed) {
		t.Fatalf("ExecuteDetached error = %v, want ErrReceiptPersistFailed", err)
	}
	if replayed {
		t.Fatal("replayed = true on a fresh run")
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result = %s, want the effect's own result (it did run)", res)
	}
	if n := countToolCalls(t, st, "t1", CallStatusExecuting); n != 1 {
		t.Fatalf("executing rows = %d, want 1 (the row must stay receipt-less 'executing')", n)
	}
	if n := countToolCalls(t, st, "t1", CallStatusFailed); n != 0 {
		t.Fatalf("failed rows = %d, want 0 ('failed' would invite a second execution)", n)
	}
	if n := eventCount(t, r, EventReceiptPersistFailed); n != 1 {
		t.Fatalf("%s events = %d, want 1", EventReceiptPersistFailed, n)
	}

	// The SAME call again (a model retrying after the error) never runs the
	// effect a second time: the live 'executing' row holds the key.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := r.ExecuteDetached(ctx, in, effect); err == nil {
		t.Fatal("second ExecuteDetached after a persist failure = nil error, want a refusal")
	}
	if runs != 1 {
		t.Fatalf("effect ran %d times, want 1 (double execution after a lost receipt)", runs)
	}

	// The resume scan sees the receipt-less W2 row and blocks the task -
	// never auto-retries it.
	if rows, err := st.Queries.ListReceiptlessToolCalls(context.Background(), "t1"); err != nil || len(rows) != 1 {
		t.Fatalf("ListReceiptlessToolCalls = (%d rows, %v), want the one stranded row", len(rows), err)
	}
}

func TestExecuteDetachedEffectErrorMarksFailedAndFreesKey(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	in := detachedInput("t1", "effect-error")
	boom := errors.New("boom")
	if _, _, err := r.ExecuteDetached(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("ExecuteDetached error = %v, want the effect's own error unwrapped", err)
	}
	if n := countToolCalls(t, st, "t1", CallStatusFailed); n != 1 {
		t.Fatalf("failed rows = %d, want 1", n)
	}
	// The effect itself failed (it reported so), so a fresh attempt may run.
	if _, replayed, err := r.ExecuteDetached(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}); err != nil || replayed {
		t.Fatalf("retry after an effect error = (replayed=%v, %v), want a fresh run", replayed, err)
	}
}

func TestExecuteDetachedRejectsClassRAndKeepsTaskLive(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	if _, _, err := r.ExecuteDetached(context.Background(), ExecuteInput{TaskID: "t1", ToolName: "fs_read", Class: ClassR, ArgsHash: "x"}, func(context.Context) (json.RawMessage, error) {
		t.Fatal("class R effect ran")
		return nil, nil
	}); !errors.Is(err, ErrReadOnlyClass) {
		t.Fatalf("ExecuteDetached(class R) = %v, want ErrReadOnlyClass", err)
	}
	var liveDuring bool
	if _, _, err := r.ExecuteDetached(context.Background(), detachedInput("t1", "live"), func(context.Context) (json.RawMessage, error) {
		liveDuring = r.IsLive("t1")
		return json.RawMessage(`{}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if !liveDuring {
		t.Fatal("IsLive(t1) during a detached effect = false: the resume scan would fail the in-flight row")
	}
	if r.IsLive("t1") {
		t.Fatal("IsLive(t1) after ExecuteDetached returned = true")
	}
}
