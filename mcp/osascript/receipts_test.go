package osascript

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	mcpfs "kahya/mcp/fs"
)

// fakeReceipts mirrors mcp/fs's receipts_test.go fake (FIX-I-4, per worker
// run since FIX-FINAL receipt-replay-per-run): only an EARLIER run's receipt
// the current run has not consumed yet is replayed (once per run); resume()
// starts the next run.
type fakeReceipts struct {
	mu                  sync.Mutex
	run                 int // current worker run; 0 means run 1
	stored              map[string][]*fakeReceipt
	executeCalls        int
	effectRuns          int
	effectCtxCancelable bool
}

type fakeReceipt struct {
	raw              json.RawMessage
	run, replayedRun int
}

func (f *fakeReceipts) key(in ReceiptInput) string {
	return in.TaskID + "|" + in.Tool + "|" + in.ArgsHash
}

func (f *fakeReceipts) resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = f.currentRunLocked() + 1
}

func (f *fakeReceipts) currentRunLocked() int {
	if f.run == 0 {
		return 1
	}
	return f.run
}

func (f *fakeReceipts) replayableLocked(in ReceiptInput) (json.RawMessage, bool) {
	run := f.currentRunLocked()
	for _, r := range f.stored[f.key(in)] {
		if r.run < run && r.replayedRun < run {
			r.replayedRun = run
			return r.raw, true
		}
	}
	return nil, false
}

func (f *fakeReceipts) Replay(_ context.Context, in ReceiptInput) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.replayableLocked(in)
	return raw, ok, nil
}

func (f *fakeReceipts) Execute(ctx context.Context, in ReceiptInput, effect func(context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	f.mu.Lock()
	f.executeCalls++
	if raw, ok := f.replayableLocked(in); ok {
		f.mu.Unlock()
		return raw, true, nil
	}
	f.effectCtxCancelable = ctx.Done() != nil
	f.mu.Unlock()
	raw, err := effect(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effectRuns++
	if err != nil {
		return nil, false, err
	}
	if f.stored == nil {
		f.stored = map[string][]*fakeReceipt{}
	}
	f.stored[f.key(in)] = append(f.stored[f.key(in)], &fakeReceipt{raw: raw, run: f.currentRunLocked()})
	return raw, false, nil
}

func newReceiptRunner() (*Runner, *fakePolicyClient, *fakeLedger, *fakeExecutor, *fakeReceipts) {
	r, pc, led, exec := newTestRunner()
	rex := &fakeReceipts{}
	r.Receipts = rex
	return r, pc, led, exec, rex
}

// TestRunApplescriptReplaysReceipt: a resumed session (the task's next
// worker run) re-issuing the SAME applescript_run of the SAME task gets the
// stored output - osascript does not run again and no second policy
// decision (approval) is taken. The same body again inside one live run
// runs (FIX-FINAL receipt-replay-per-run).
func TestRunApplescriptReplaysReceipt(t *testing.T) {
	r, pc, led, exec, rex := newReceiptRunner()
	in := ScriptInput{Script: `tell application "Notes" to make new note`, TargetApp: "Notes"}

	out1, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if live, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in); err != nil || live.Replayed {
		t.Fatalf("live repeat = (%+v, %v), want a fresh run", live, err)
	}
	rex.resume()
	out2, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("second (resumed) run: %v", err)
	}
	if !out2.Replayed || out2.ReplayNote != mcpfs.MsgReplayedFromEarlierRun {
		t.Fatalf("resumed run = %+v, want it answered from its receipt with the replay note", out2)
	}
	out2.Replayed, out2.ReplayNote = false, ""
	if out2.ExitCode != out1.ExitCode || out2.Stdout != out1.Stdout || out2.Stderr != out1.Stderr {
		t.Fatalf("replayed output = %+v, want the stored %+v", out2, out1)
	}
	if n := exec.callCount(); n != 2 {
		t.Fatalf("osascript invocations = %d, want 2 (run 1's two; the resumed call replayed)", n)
	}
	if pc.checkCalls != 2 || pc.consumeCalls != 2 {
		t.Fatalf("Check/ConsumeToken = %d/%d, want 2/2 (the replay takes no decision)", pc.checkCalls, pc.consumeCalls)
	}
	if got := len(led.find("osascript_exec")); got != 2 {
		t.Fatalf("osascript_exec rows = %d, want 2", got)
	}
	if got := len(led.find("osascript_replayed")); got != 1 {
		t.Fatalf("osascript_replayed rows = %d, want 1", got)
	}
	if rex.effectCtxCancelable {
		t.Fatal("osascript ran on a cancelable context (must be detached from the MCP request)")
	}
}

// TestRunJXAReceiptIsPerTool: the same body run as applescript_run and as
// jxa_run are different calls (the tool is part of the receipt key).
func TestRunJXAReceiptIsPerTool(t *testing.T) {
	r, _, _, exec, _ := newReceiptRunner()
	in := ScriptInput{Script: `Application("Notes").notes.length`}
	if _, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in); err != nil {
		t.Fatal(err)
	}
	out, err := r.RunJXA(context.Background(), "trace-1", "task-1", in)
	if err != nil || out.Replayed {
		t.Fatalf("jxa_run after an identical applescript_run = (%+v, %v), want a fresh run", out, err)
	}
	if n := exec.callCount(); n != 2 {
		t.Fatalf("osascript invocations = %d, want 2", n)
	}
}

// TestRunApplescriptTimeoutWritesNoReceipt: a timed-out run is a failed
// effect - the caller still gets TimedOut + the Turkish timeout error, and
// nothing is stored (a later identical call may run once approved again).
func TestRunApplescriptTimeoutWritesNoReceipt(t *testing.T) {
	r, _, _, exec, rex := newReceiptRunner()
	exec.blockUntilCtxDone = true
	out, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{Script: `delay 999`})
	if err == nil || err.Error() != reasonTimeout {
		t.Fatalf("err = %v, want %q", err, reasonTimeout)
	}
	if !out.TimedOut {
		t.Fatal("out.TimedOut = false, want true")
	}
	if len(rex.stored) != 0 {
		t.Fatalf("stored receipts = %d, want 0 after a timeout", len(rex.stored))
	}
}

// TestRunApplescriptAutomationDeniedKeepsOutputAndTypedError: the typed TCC
// error and the partial output survive the receipt wrapper; no receipt.
func TestRunApplescriptAutomationDeniedKeepsOutputAndTypedError(t *testing.T) {
	r, _, _, exec, rex := newReceiptRunner()
	exec.result = Result{ExitCode: 1, Stderr: []byte("execution error: Not authorized to send Apple events to Finder. (-1743)")}
	out, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{Script: `tell application "Finder" to get name of home`, TargetApp: "Finder"})
	var tccErr *AutomationDeniedError
	if !errors.As(err, &tccErr) {
		t.Fatalf("err = %v (%T), want *AutomationDeniedError", err, err)
	}
	if out.ExitCode != 1 || out.Stderr == "" {
		t.Fatalf("out = %+v, want the denied run's exit code and stderr", out)
	}
	if len(rex.stored) != 0 {
		t.Fatal("a denied run stored a receipt")
	}
}

func TestRunShortcutReplaysReceipt(t *testing.T) {
	r, pc, led, exec, rex := newReceiptRunner()
	in := ShortcutInput{Name: "Günlük Not"}
	if _, err := r.RunShortcut(context.Background(), "trace-1", "task-1", in); err != nil {
		t.Fatalf("first RunShortcut: %v", err)
	}
	rex.resume()
	out, err := r.RunShortcut(context.Background(), "trace-1", "task-1", in)
	if err != nil || !out.Replayed || out.ReplayNote != mcpfs.MsgReplayedFromEarlierRun {
		t.Fatalf("second (resumed) RunShortcut = (%+v, %v), want a replay with the note", out, err)
	}
	if n := exec.callCount(); n != 1 {
		t.Fatalf("shortcuts invocations = %d, want 1", n)
	}
	if pc.checkCalls != 1 {
		t.Fatalf("Check calls = %d, want 1", pc.checkCalls)
	}
	if got := len(led.find("shortcuts_replayed")); got != 1 {
		t.Fatalf("shortcuts_replayed rows = %d, want 1", got)
	}
}
