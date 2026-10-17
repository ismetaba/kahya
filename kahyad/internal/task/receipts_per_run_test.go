package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// FIX-FINAL receipt-replay-per-run (owner decision 2026-10-16): receipts
// replay only on resume - a receipt answers a call of a LATER worker run of
// the task, at most once per run, and never a repeat inside the live run
// that wrote it.

// countingTool is a stub side effect whose result names its invocation, so
// a test can tell WHICH earlier execution a replay answered with.
type countingTool struct{ n int }

func (c *countingTool) effect(label string) DetachedEffectFunc {
	return func(context.Context) (json.RawMessage, error) {
		c.n++
		return json.Marshal(map[string]any{"call": label, "invocation": c.n})
	}
}

func perRunInput(taskID, args string) ExecuteInput {
	return ExecuteInput{TaskID: taskID, TraceID: "trace-" + taskID, ToolName: "fs_write", Class: ClassW1, ArgsHash: HashArgs([]byte(args))}
}

type callOutcome struct {
	result   string
	replayed bool
}

func runCall(t *testing.T, r *Receipts, in ExecuteInput, tool *countingTool, label string) callOutcome {
	t.Helper()
	res, replayed, err := r.ExecuteDetached(context.Background(), in, tool.effect(label))
	if err != nil {
		t.Fatalf("ExecuteDetached(%s): %v", label, err)
	}
	return callOutcome{result: string(res), replayed: replayed}
}

func toolCallRuns(t *testing.T, st *store.Store, taskID string) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT args_hash, run, replayed_run, status FROM tool_calls WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash, status string
		var run, replayed int64
		if err := rows.Scan(&hash, &run, &replayed, &status); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s:%d:%d:%s", hash[:4], run, replayed, status))
	}
	return out
}

// TestLiveRepeatInsideOneRunExecutesEveryTime: X, Y, X in one worker run -
// all three execute (the third is NOT answered from the first's receipt),
// three receipt rows, no tool.replayed.
func TestLiveRepeatInsideOneRunExecutesEveryTime(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	beginWorkerRun(t, st, "t1")
	r := newReceipts(st)
	tool := &countingTool{}
	x, y := perRunInput("t1", "X"), perRunInput("t1", "Y")
	for _, c := range []struct {
		in    ExecuteInput
		label string
	}{{x, "x1"}, {y, "y1"}, {x, "x2"}} {
		if got := runCall(t, r, c.in, tool, c.label); got.replayed {
			t.Fatalf("%s replayed inside the live run that wrote the receipt: %+v", c.label, got)
		}
	}
	if tool.n != 3 {
		t.Fatalf("effects = %d, want 3", tool.n)
	}
	if n := countToolCalls(t, st, "t1", CallStatusReceipt); n != 3 {
		t.Fatalf("receipt rows = %d, want 3", n)
	}
	if n := eventCount(t, r, EventReplayed); n != 0 {
		t.Fatalf("tool.replayed = %d, want 0", n)
	}
	// Replay() agrees: nothing to replay for the run that wrote them.
	if _, hit, err := r.Replay(context.Background(), x); err != nil || hit {
		t.Fatalf("Replay(X) in run 1 = (hit=%v, %v), want a miss", hit, err)
	}
}

// TestResumeReplaysEachEarlierReceiptOncePerRun: run 1 completed X; run 2's
// first X replays it, run 2's second X executes (the receipt is consumed for
// run 2); run 3 replays BOTH earlier X receipts once each, oldest first, and
// then executes.
func TestResumeReplaysEachEarlierReceiptOncePerRun(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &countingTool{}
	x := perRunInput("t1", "X")

	beginWorkerRun(t, st, "t1")
	r1 := runCall(t, r, x, tool, "run1")

	beginWorkerRun(t, st, "t1")
	if got := runCall(t, r, x, tool, "run2-a"); !got.replayed || got.result != r1.result {
		t.Fatalf("run 2 first X = %+v, want run 1's %s replayed", got, r1.result)
	}
	r2 := runCall(t, r, x, tool, "run2-b")
	if r2.replayed {
		t.Fatalf("run 2 second X replayed again: each earlier receipt is consumed at most once per run")
	}

	beginWorkerRun(t, st, "t1")
	for i, want := range []string{r1.result, r2.result} {
		if got := runCall(t, r, x, tool, fmt.Sprintf("run3-%d", i)); !got.replayed || got.result != want {
			t.Fatalf("run 3 X #%d = %+v, want %s replayed", i+1, got, want)
		}
	}
	if got := runCall(t, r, x, tool, "run3-last"); got.replayed {
		t.Fatalf("run 3 third X = %+v, want an execution (both earlier receipts consumed)", got)
	}
	if tool.n != 3 {
		t.Fatalf("effects = %d, want 3 (run1, run2-b, run3-last)", tool.n)
	}
	if got := toolCallRuns(t, st, "t1"); len(got) != 3 {
		t.Fatalf("tool_calls = %v, want 3 rows (replays add none)", got)
	}
}

// TestCrashAfterXYXResumeReplaysThreeAndExecutesFourth: run 1 did X, Y, X
// and then crashed; the resumed run re-issuing X, Y, X gets the three
// receipts (the two X in order), and a 4th X executes.
func TestCrashAfterXYXResumeReplaysThreeAndExecutesFourth(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	tool := &countingTool{}
	x, y := perRunInput("t1", "X"), perRunInput("t1", "Y")

	beginWorkerRun(t, st, "t1")
	x1 := runCall(t, r, x, tool, "x1")
	y1 := runCall(t, r, y, tool, "y1")
	x2 := runCall(t, r, x, tool, "x2")

	beginWorkerRun(t, st, "t1") // crash + resume
	for i, c := range []struct {
		in   ExecuteInput
		want string
	}{{x, x1.result}, {y, y1.result}, {x, x2.result}} {
		if got := runCall(t, r, c.in, tool, fmt.Sprintf("replay-%d", i)); !got.replayed || got.result != c.want {
			t.Fatalf("resumed call #%d = %+v, want %s replayed", i+1, got, c.want)
		}
	}
	if got := runCall(t, r, x, tool, "x4"); got.replayed {
		t.Fatalf("4th X after the resume = %+v, want an execution", got)
	}
	if tool.n != 4 {
		t.Fatalf("effects = %d, want 4 (x1, y1, x2, x4)", tool.n)
	}
	if n := eventCount(t, r, EventReplayed); n != 3 {
		t.Fatalf("tool.replayed = %d, want exactly 3", n)
	}
}

// TestRunPinKeepsOneCallOnTheRunThatIssuedIt: a call whose task is
// redispatched (tasks.worker_run bumped) between its Replay and its Execute -
// e.g. while it waits on an approval - is recorded under the run that issued
// it when the call carries a run pin, so the later run replays it instead of
// executing it a second time. Without a pin each step reads the row anew.
func TestRunPinKeepsOneCallOnTheRunThatIssuedIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pin     bool
		wantRun int64
	}{{"pinned", true, 1}, {"unpinned", false, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			insertTask(t, st, "t1")
			beginWorkerRun(t, st, "t1")
			r := newReceipts(st)
			ctx := context.Background()
			if tc.pin {
				ctx = WithRunPin(ctx)
			}
			in := perRunInput("t1", "approval-wait")
			if _, hit, err := r.Replay(ctx, in); err != nil || hit {
				t.Fatalf("Replay = (hit=%v, %v), want a miss", hit, err)
			}
			beginWorkerRun(t, st, "t1") // redispatched while the call waited
			tool := &countingTool{}
			if _, replayed, err := r.ExecuteDetached(ctx, in, tool.effect("issued-by-run-1")); err != nil || replayed {
				t.Fatalf("ExecuteDetached = (replayed=%v, %v)", replayed, err)
			}
			var run int64
			if err := st.DB().QueryRow(`SELECT run FROM tool_calls WHERE task_id = 't1'`).Scan(&run); err != nil {
				t.Fatal(err)
			}
			if run != tc.wantRun {
				t.Fatalf("tool_calls.run = %d, want %d", run, tc.wantRun)
			}
			if !tc.pin {
				return
			}
			// Run 2 re-issues the call its predecessor issued: replayed.
			if _, hit, err := r.Replay(context.Background(), in); err != nil || !hit {
				t.Fatalf("run 2 Replay = (hit=%v, %v), want run 1's receipt replayed", hit, err)
			}
		})
	}
}

// failingRunStore is the real store with the worker-run read failing.
type failingRunStore struct {
	ToolCallStore
}

func (failingRunStore) GetTaskWorkerRun(context.Context, string) (int64, error) {
	return 0, errors.New("database is locked (injected)")
}

// TestWorkerRunLookupErrorFailsClosed: when the task's run cannot be read,
// Replay and Execute refuse - nothing is replayed, claimed or executed.
func TestWorkerRunLookupErrorFailsClosed(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	r := newReceipts(st)
	r.q = failingRunStore{ToolCallStore: st.Queries}
	in := perRunInput("t1", "run-lookup")
	if _, hit, err := r.Replay(context.Background(), in); err == nil || hit {
		t.Fatalf("Replay with a failing run lookup = (hit=%v, %v), want a refusal", hit, err)
	}
	ran := false
	if _, _, err := r.ExecuteDetached(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		ran = true
		return json.RawMessage(`{}`), nil
	}); err == nil || ran {
		t.Fatalf("ExecuteDetached with a failing run lookup = (ran=%v, %v), want a refusal before the effect", ran, err)
	}
	if rows := toolCallRuns(t, st, "t1"); len(rows) != 0 {
		t.Fatalf("tool_calls after refusals = %v, want none", rows)
	}
}

// TestUnknownTaskRunIsZeroAndNeverReplays: a task id with no tasks row has
// no tool_calls rows (FK), so its run is 0 and Replay is a clean miss; an
// Execute then fails closed on the FK without running the effect.
func TestUnknownTaskRunIsZeroAndNeverReplays(t *testing.T) {
	st := testStore(t)
	r := newReceipts(st)
	in := perRunInput("no-such-task", "x")
	if _, hit, err := r.Replay(context.Background(), in); err != nil || hit {
		t.Fatalf("Replay(unknown task) = (hit=%v, %v), want a clean miss", hit, err)
	}
	ran := false
	if _, _, err := r.ExecuteDetached(context.Background(), in, func(context.Context) (json.RawMessage, error) {
		ran = true
		return nil, nil
	}); err == nil || ran {
		t.Fatalf("ExecuteDetached(unknown task) = (ran=%v, %v), want the FK refusal before the effect", ran, err)
	}
}

// TestW1RetryCapIgnoresCompletedLiveRepeats: completed live repeats of an
// identical W1 call (many receipts of one key) never count against
// task.retry.w1_max_auto - one interrupted attempt after five completed
// ones is auto-retried, not blocked. Interrupted attempts after the latest
// receipt still count, so the cap still ends a crash loop.
func TestW1RetryCapIgnoresCompletedLiveRepeats(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace-t1", "t1", StatusExecuting); err != nil {
		t.Fatal(err)
	}
	beginWorkerRun(t, st, "t1")
	r := newReceipts(st)
	tool := &countingTool{}
	in := perRunInput("t1", "repeat")
	for i := 0; i < 5; i++ {
		runCall(t, r, in, tool, fmt.Sprintf("live-%d", i))
	}
	insertReceiptlessToolCall(t, st, "t1", in.ToolName, string(ClassW1), in.ArgsHash)

	notifier := &fakeNotifier{}
	resume := newResume(st, m, notifier, 3)
	task, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := resume.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Fatalf("status = %q, want executing (one interruption after five completed repeats is within the cap)", got)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("notices = %+v, want none", notifier.calls)
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Fatalf("resume rows = %d, want 1 (auto-retry requeued)", n)
	}

	// Interruptions after the latest receipt still add up to the cap.
	attempts, err := st.Queries.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: in.ToolName, ArgsHash: in.ArgsHash})
	if err != nil || attempts != 1 {
		t.Fatalf("CountToolCallAttempts after 5 receipts + 1 interruption = (%d, %v), want 1", attempts, err)
	}
	for i := 0; i < 3; i++ {
		insertReceiptlessToolCall(t, st, "t1", in.ToolName, string(ClassW1), in.ArgsHash)
		if err := st.Queries.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{FinishedAt: sql.NullString{String: "2026-01-01T00:00:00Z", Valid: true}, ID: lastToolCallID(t, st)}); err != nil {
			t.Fatal(err)
		}
	}
	attempts, err = st.Queries.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: in.ToolName, ArgsHash: in.ArgsHash})
	if err != nil || attempts != 4 {
		t.Fatalf("CountToolCallAttempts after 3 more interruptions = (%d, %v), want 4", attempts, err)
	}
	// A call of the same run that completes resets the count: the key's
	// latest attempt is a receipt again. Another key's receipt does not.
	runCall(t, r, perRunInput("t1", "other"), tool, "other")
	if attempts, err := st.Queries.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: in.ToolName, ArgsHash: in.ArgsHash}); err != nil || attempts != 4 {
		t.Fatalf("CountToolCallAttempts after another key's receipt = (%d, %v), want still 4", attempts, err)
	}
	if got := runCall(t, r, in, tool, "done"); got.replayed {
		t.Fatalf("live repeat in run 1 replayed: %+v", got)
	}
	attempts, err = st.Queries.CountToolCallAttempts(ctx, sqlcgen.CountToolCallAttemptsParams{TaskID: "t1", ToolName: in.ToolName, ArgsHash: in.ArgsHash})
	if err != nil || attempts != 0 {
		t.Fatalf("CountToolCallAttempts after a later receipt = (%d, %v), want 0", attempts, err)
	}
}

func lastToolCallID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRow(`SELECT MAX(id) FROM tool_calls`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
