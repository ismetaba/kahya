// acceptance_test.go implements the W4-02 task spec's two acceptance
// integration tests, verbatim:
//
//	(a) task runs a stub W2 tool whose kahyad-side execution COMPLETES
//	    while the (stub) worker is SIGKILLed mid-call -> after dispatcher
//	    resume, tool_calls receipt count=1, stub side-effect counter=1,
//	    and a tool.replayed event exists.
//	(b) same scenario but the tool NEVER wrote a receipt -> task row is
//	    blocked_user, notification event payload has the exact Turkish
//	    string, and `kahya task resolve <id> --abort` moves it to failed.
//
// Both are explicitly framed by the spec as "the CI-speed precursor of
// the W4-07 gate" (which drives a REAL claude-agent-sdk worker and a real
// SIGKILL) - neither test here spawns a real Claude session. (a) instead:
// runs the stub tool's effect directly through kahyad/internal/task.
// Receipts (standing in for "kahyad-side execution" - side-effectful
// tools are kahyad-owned, HANDOFF §5 safety #1 enforcement plane, so a
// dead WORKER process does not imply a dead tool execution); leaves the
// task 'executing' with no worker ever having reported completion
// (standing in for "the worker was SIGKILLed mid-call"); runs the resume
// scan + a REAL Dispatcher.ClaimAndDispatch pass, which re-spawns a fake
// worker script and drives the task to 'done'; and finally calls
// Receipts.Execute a SECOND time for the exact same call (standing in for
// "the resumed session re-attempts the interrupted tool call") to prove
// the replay path, not the effect, is what answers it.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// stubW2Tool mirrors kahyad/internal/task's own receipts_test.go stub (a
// real fs/shell/osascript tool wiring is out of this task's scope - see
// this task's own deviations note); Invocations counts how many times the
// tool's SIDE EFFECT actually ran.
type stubW2Tool struct {
	Invocations int
}

func (s *stubW2Tool) effect(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
	s.Invocations++
	return json.Marshal(map[string]string{"status": "moved-to-trash"})
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func taskStatusOfStore(t *testing.T, st *store.Store, taskID string) string {
	t.Helper()
	row, err := st.Queries.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row.Status
}

func insertReceiptlessCall(t *testing.T, st *store.Store, taskID, tool, class, argsHash string) sqlcgen.ToolCall {
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

// TestAcceptanceReceiptedCallSurvivesResumeWithoutDoubleExecution is
// acceptance criterion (a).
func TestAcceptanceReceiptedCallSurvivesResumeWithoutDoubleExecution(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const taskID = "t_w2_receipted"
	const tool = "stub_delete"
	argsHash := task.HashArgs([]byte("/Users/matt/notes/todo.txt"))

	// The task was spawned normally and had already reported a session
	// before the worker was SIGKILLed.
	insertTaskWithEnvelope(t, st, taskID, "sess-before-kill")

	// kahyad-side execution of the W2 tool call COMPLETES: intent ->
	// executing -> receipt, all the way through, exactly as it would if
	// the worker died a moment later (or even before) seeing the result.
	receipts := task.NewReceipts(st.DB(), st.Queries, st)
	stub := &stubW2Tool{}
	if _, replayed, err := receipts.Execute(ctx, task.ExecuteInput{
		TaskID: taskID, TraceID: "trace-" + taskID, ToolName: tool, Class: task.ClassW2, ArgsHash: argsHash,
	}, stub.effect); err != nil {
		t.Fatalf("Execute() error = %v", err)
	} else if replayed {
		t.Fatal("Execute() replayed = true on the very first call")
	}
	if stub.Invocations != 1 {
		t.Fatalf("Invocations after first Execute = %d, want 1", stub.Invocations)
	}
	if n := countRows(t, st, `SELECT count(*) FROM tool_calls WHERE task_id = ? AND status = 'receipt'`, taskID); n != 1 {
		t.Fatalf("receipt-status tool_calls = %d, want 1", n)
	}

	// The (stub) worker is SIGKILLed mid-call: no session/result line ever
	// arrives, the task row is simply left at 'executing' with no live
	// worker (this test never registers one with a LiveRegistry, matching
	// the kahyad-startup-scan scenario exactly - see LiveChecker's own doc
	// comment: nil live checker = every 'executing' task is not-live).
	m := task.NewMachine(st.Queries, st)
	notifier := notify.New(nil, st)
	resume := task.NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 3)
	got, err := st.Queries.GetTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := resume.ProcessTask(ctx, got); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	// No receipt-less call existed (it already completed) -> requeued
	// directly, task stays executing.
	if s := taskStatusOfStore(t, st, taskID); s != task.StatusExecuting {
		t.Fatalf("status after resume scan = %q, want %q", s, task.StatusExecuting)
	}

	// The outbox dispatcher claims the resume row and re-spawns a (fake)
	// worker, which reports the SAME session_id and a clean "ok" result -
	// this is "after dispatcher resume".
	dispatcher := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})
	claimed, err := dispatcher.ClaimAndDispatch(ctx)
	if err != nil {
		t.Fatalf("ClaimAndDispatch: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if s := taskStatusOfStore(t, st, taskID); s != task.StatusDone {
		t.Fatalf("status after dispatcher resume = %q, want %q", s, task.StatusDone)
	}

	// The resumed session re-attempts the interrupted tool call (the ONE
	// thing a real Claude session inside the resumed worker would do that
	// this test cannot literally drive - see this file's own doc
	// comment): Receipts.Execute must replay the stored receipt, NEVER
	// re-run the stub's side effect.
	result, replayed, err := receipts.Execute(ctx, task.ExecuteInput{
		TaskID: taskID, TraceID: "trace-" + taskID, ToolName: tool, Class: task.ClassW2, ArgsHash: argsHash,
	}, stub.effect)
	if err != nil {
		t.Fatalf("Execute() (post-resume) error = %v", err)
	}
	if !replayed {
		t.Fatal("Execute() (post-resume) replayed = false, want true")
	}
	if stub.Invocations != 1 {
		t.Fatalf("Invocations after post-resume Execute = %d, want STILL 1 (must not re-execute)", stub.Invocations)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result, &decoded); err != nil || decoded["status"] != "moved-to-trash" {
		t.Errorf("replayed result = %s, decode err = %v", result, err)
	}

	// Acceptance criterion's own three checks, verbatim:
	if n := countRows(t, st, `SELECT count(*) FROM tool_calls WHERE task_id = ? AND status = 'receipt'`, taskID); n != 1 {
		t.Errorf("tool_calls receipt count = %d, want 1", n)
	}
	if stub.Invocations != 1 {
		t.Errorf("stub side-effect counter = %d, want 1", stub.Invocations)
	}
	if n := countRows(t, st, `SELECT count(*) FROM events WHERE kind = ?`, task.EventReplayed); n != 1 {
		t.Errorf("tool.replayed events = %d, want 1", n)
	}
}

// TestAcceptanceReceiptlessCallBlocksUserThenAborts is acceptance
// criterion (b).
func TestAcceptanceReceiptlessCallBlocksUserThenAborts(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const taskID = "t_w2_receiptless"
	const tool = "stub_delete"

	insertTaskWithEnvelope(t, st, taskID, "sess-before-kill")

	// kahyad-side execution NEVER completed: a tool_calls row exists at
	// 'executing' but the side effect never finished (or kahyad itself
	// died mid-effect) - no receipt was ever written.
	insertReceiptlessCall(t, st, taskID, tool, "W2", task.HashArgs([]byte("/Users/matt/notes/secret.txt")))

	m := task.NewMachine(st.Queries, st)
	notifier := notify.New(nil, st)
	resume := task.NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 3)

	got, err := st.Queries.GetTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if err := resume.ProcessTask(ctx, got); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if s := taskStatusOfStore(t, st, taskID); s != task.StatusBlockedUser {
		t.Fatalf("status = %q, want %q", s, task.StatusBlockedUser)
	}

	var payloadJSON string
	if err := st.DB().QueryRow(
		`SELECT payload FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, task.EventW2W3Receiptless,
	).Scan(&payloadJSON); err != nil {
		t.Fatalf("query events: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	wantMsg := fmt.Sprintf(task.MsgW2W3ReceiptlessFmt, taskID, tool, taskID)
	if got, _ := decoded["message"].(string); got != wantMsg {
		t.Fatalf("notification payload message = %q, want the exact Turkish string %q", got, wantMsg)
	}

	// `kahya task resolve <id> --abort` -> failed.
	resolver := task.NewResolver(st.Queries, st.Queries, m)
	if err := resolver.Abort(ctx, "trace-"+taskID, taskID); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
	if s := taskStatusOfStore(t, st, taskID); s != task.StatusFailed {
		t.Fatalf("status after --abort = %q, want %q", s, task.StatusFailed)
	}
}
