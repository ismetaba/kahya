package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// fakeNotifier records every Notify call - tests assert exact Turkish
// message text against the recorded message, in addition to checking the
// real ledger event a genuine notify.JSONLNotifier would have written
// (see TestW2ReceiptlessBlocksUserWithRealNotifier).
type fakeNotifier struct {
	calls []notifyCall
}

type notifyCall struct {
	traceID, kind, message string
	payload                map[string]any
}

func (f *fakeNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	f.calls = append(f.calls, notifyCall{traceID: traceID, kind: kind, message: message, payload: payload})
	return nil
}

// insertReceiptlessToolCall inserts one tool_calls row stuck at
// 'executing' (an interrupted side-effectful call, never marked receipt
// or failed) for (taskID, tool, class, argsHash) at the next seq for that
// triple.
func insertReceiptlessToolCall(t *testing.T, st *store.Store, taskID, tool, class, argsHash string) sqlcgen.ToolCall {
	t.Helper()
	ctx := context.Background()
	seq, err := st.Queries.NextToolCallSeq(ctx, sqlcgen.NextToolCallSeqParams{TaskID: taskID, ToolName: tool, ArgsHash: argsHash})
	if err != nil {
		t.Fatalf("NextToolCallSeq: %v", err)
	}
	row, err := st.Queries.InsertToolCallIntent(ctx, sqlcgen.InsertToolCallIntentParams{
		TaskID: taskID, Seq: seq, ToolName: tool, Class: class, ArgsHash: argsHash, CreatedAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertToolCallIntent: %v", err)
	}
	if err := st.Queries.MarkToolCallExecuting(ctx, sqlcgen.MarkToolCallExecutingParams{
		StartedAt: sql.NullString{String: "2026-01-01T00:00:00Z", Valid: true}, ID: row.ID,
	}); err != nil {
		t.Fatalf("MarkToolCallExecuting: %v", err)
	}
	return row
}

// setupTaskWithReceiptlessCall inserts a task (transitioned to
// 'executing') plus one receipt-less tool_calls row - the scenario the
// resume scan's ProcessTask is built to resolve.
func setupTaskWithReceiptlessCall(t *testing.T, st *store.Store, taskID, tool, class, argsHash string) {
	t.Helper()
	insertTask(t, st, taskID)
	m := NewMachine(st.Queries, st)
	if err := m.Transition(context.Background(), "trace-"+taskID, taskID, StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	insertReceiptlessToolCall(t, st, taskID, tool, class, argsHash)
}

func newResume(st *store.Store, m *Machine, notifier Notifier, w1Max int) *Resume {
	return NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, w1Max)
}

func countOutboxResumeRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox WHERE kind = ?`, OutboxKindTaskResume).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

func TestResumeNoReceiptlessCallRequeuesDirectly(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace-t1", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	notifier := &fakeNotifier{}
	r := newResume(st, m, notifier, 3)

	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := r.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	// Status must remain 'executing' - no transition needed for a clean
	// resume.
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q", got, StatusExecuting)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notify calls = %d, want 0", len(notifier.calls))
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Errorf("outbox task_resume rows = %d, want 1", n)
	}
}

// TestW1ReceiptlessAutoRetriesWithinCap is the spec's explicit
// "W1 receipt-less auto-retries exactly once more (attempts=2)" test.
func TestW1ReceiptlessAutoRetriesWithinCap(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	setupTaskWithReceiptlessCall(t, st, "t1", "stub_write", "W1", HashArgs([]byte("args")))
	m := NewMachine(st.Queries, st)
	notifier := &fakeNotifier{}
	r := newResume(st, m, notifier, 3)

	// After setup, attempts == 1 (the initial intent->executing dispatch).
	if got := taskAttempts(t, st, "t1"); got != 1 {
		t.Fatalf("attempts before resume = %d, want 1", got)
	}

	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := r.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q (within cap - stays executing)", got, StatusExecuting)
	}
	if got := taskAttempts(t, st, "t1"); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if n := countToolCalls(t, st, "t1", CallStatusFailed); n != 1 {
		t.Errorf("failed tool_calls = %d, want 1 (the interrupted row)", n)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notify calls = %d, want 0 (still within cap, no blocked_user)", len(notifier.calls))
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Errorf("outbox task_resume rows = %d, want 1 (auto-retry requeued)", n)
	}
}

// TestW1PastCapBlocksUserWithExactMessage is the spec's explicit
// "W1 killed past w1_max_auto => blocked_user + the exact W1-cap Turkish
// string" test.
func TestW1PastCapBlocksUserWithExactMessage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const w1Max = 3
	tool, argsHash := "stub_write", HashArgs([]byte("args"))
	setupTaskWithReceiptlessCall(t, st, "t1", tool, "W1", argsHash)
	m := NewMachine(st.Queries, st)
	notifier := &fakeNotifier{}
	r := newResume(st, m, notifier, w1Max)

	// Drive w1Max within-cap auto-retries (this task already has ONE
	// receipt-less attempt from setup, processed as the loop's first
	// iteration); the (w1Max+1)th attempt, processed after the loop, tips
	// it over.
	for i := 0; i < w1Max; i++ {
		task, err := st.Queries.GetTaskByID(ctx, "t1")
		if err != nil {
			t.Fatalf("GetTaskByID: %v", err)
		}
		if err := r.ProcessTask(ctx, task); err != nil {
			t.Fatalf("ProcessTask (iteration %d): %v", i, err)
		}
		// Re-arm: another receipt-less attempt for the SAME triple - what
		// a real dispatcher-driven redispatch would produce in production
		// via Receipts.Execute; the test inserts it directly.
		insertReceiptlessToolCall(t, st, "t1", tool, "W1", argsHash)
	}

	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := r.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask (final): %v", err)
	}

	if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
		t.Fatalf("status = %q, want %q", got, StatusBlockedUser)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("notify calls = %d, want 1", len(notifier.calls))
	}
	call := notifier.calls[0]
	wantN := int64(w1Max + 1)
	wantMsg := fmt.Sprintf(MsgW1RetryCapExceededFmt, "t1", tool, wantN, "t1")
	if call.message != wantMsg {
		t.Errorf("message = %q, want %q", call.message, wantMsg)
	}
	if call.kind != EventW1RetryCapExceeded {
		t.Errorf("kind = %q, want %q", call.kind, EventW1RetryCapExceeded)
	}
}

// TestW2ReceiptlessBlocksUserWithExactMessage is the spec's explicit
// "W2 receipt-less => blocked_user + notification event with the exact
// W2/W3 Turkish string" test.
func TestW2ReceiptlessBlocksUserWithExactMessage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tool := "stub_docker"
	setupTaskWithReceiptlessCall(t, st, "t1", tool, "W2", HashArgs([]byte("args")))
	m := NewMachine(st.Queries, st)
	notifier := &fakeNotifier{}
	r := newResume(st, m, notifier, 3)

	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := r.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
		t.Fatalf("status = %q, want %q", got, StatusBlockedUser)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("notify calls = %d, want 1", len(notifier.calls))
	}
	call := notifier.calls[0]
	wantMsg := fmt.Sprintf(MsgW2W3ReceiptlessFmt, "t1", tool, "t1")
	if call.message != wantMsg {
		t.Errorf("message = %q, want %q", call.message, wantMsg)
	}
	if call.kind != EventW2W3Receiptless {
		t.Errorf("kind = %q, want %q", call.kind, EventW2W3Receiptless)
	}
	if n := countToolCalls(t, st, "t1", CallStatusFailed); n != 1 {
		t.Errorf("failed tool_calls = %d, want 1", n)
	}
}

// TestW2ReceiptlessBlocksUserWithRealNotifier exercises the real
// kahyad/internal/notify.JSONLNotifier (not the test fake above) end to
// end, proving the events table row for EventW2W3Receiptless actually
// carries payload.message == the exact Turkish string - the literal
// acceptance-criterion wording ("notification event payload contains the
// exact Turkish string").
func TestW2ReceiptlessBlocksUserWithRealNotifier(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tool := "stub_docker"
	setupTaskWithReceiptlessCall(t, st, "t1", tool, "W3", HashArgs([]byte("args")))
	m := NewMachine(st.Queries, st)
	r := NewResume(st.Queries, st.Queries, st.Queries, m, jsonlLikeNotifier{st}, nil, 3)

	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := r.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	var payloadJSON string
	if err := st.DB().QueryRow(
		`SELECT payload FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, EventW2W3Receiptless,
	).Scan(&payloadJSON); err != nil {
		t.Fatalf("query events: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	want := fmt.Sprintf(MsgW2W3ReceiptlessFmt, "t1", tool, "t1")
	if got, _ := decoded["message"].(string); got != want {
		t.Errorf("payload.message = %q, want %q", got, want)
	}
}

// jsonlLikeNotifier reproduces kahyad/internal/notify.JSONLNotifier's
// exact observable Notify contract (ledger a row whose payload carries
// "message") against this package's own Ledger dependency, without this
// test needing to import kahyad/internal/notify (which would otherwise
// be this package's only non-test dependent, purely to satisfy one
// test's type).
type jsonlLikeNotifier struct{ ledger Ledger }

func (n jsonlLikeNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	full := map[string]any{"message": message}
	for k, v := range payload {
		full[k] = v
	}
	return n.ledger.LogEvent(ctx, traceID, kind, full)
}
