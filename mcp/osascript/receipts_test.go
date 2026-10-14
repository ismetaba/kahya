package osascript

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// fakeReceipts mirrors mcp/fs's receipts_test.go fake (FIX-I-4).
type fakeReceipts struct {
	mu                  sync.Mutex
	stored              map[string]json.RawMessage
	executeCalls        int
	effectRuns          int
	effectCtxCancelable bool
}

func (f *fakeReceipts) key(in ReceiptInput) string {
	return in.TaskID + "|" + in.Tool + "|" + in.ArgsHash
}

func (f *fakeReceipts) Replay(_ context.Context, in ReceiptInput) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.stored[f.key(in)]
	return raw, ok, nil
}

func (f *fakeReceipts) Execute(ctx context.Context, in ReceiptInput, effect func(context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	f.mu.Lock()
	f.executeCalls++
	if raw, ok := f.stored[f.key(in)]; ok {
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
		f.stored = map[string]json.RawMessage{}
	}
	f.stored[f.key(in)] = raw
	return raw, false, nil
}

func newReceiptRunner() (*Runner, *fakePolicyClient, *fakeLedger, *fakeExecutor, *fakeReceipts) {
	r, pc, led, exec := newTestRunner()
	rex := &fakeReceipts{}
	r.Receipts = rex
	return r, pc, led, exec, rex
}

// TestRunApplescriptReplaysReceipt: a resumed session re-issuing the SAME
// applescript_run of the SAME task gets the stored output - osascript does
// not run again and no second policy decision (approval) is taken.
func TestRunApplescriptReplaysReceipt(t *testing.T) {
	r, pc, led, exec, rex := newReceiptRunner()
	in := ScriptInput{Script: `tell application "Notes" to make new note`, TargetApp: "Notes"}

	out1, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	out2, err := r.RunApplescript(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("second (resumed) run: %v", err)
	}
	if !out2.Replayed {
		t.Fatal("second identical run was not answered from its receipt")
	}
	out2.Replayed = false
	if out2.ExitCode != out1.ExitCode || out2.Stdout != out1.Stdout || out2.Stderr != out1.Stderr {
		t.Fatalf("replayed output = %+v, want the stored %+v", out2, out1)
	}
	if n := exec.callCount(); n != 1 {
		t.Fatalf("osascript invocations = %d, want 1", n)
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("Check/ConsumeToken = %d/%d, want 1/1", pc.checkCalls, pc.consumeCalls)
	}
	if got := len(led.find("osascript_exec")); got != 1 {
		t.Fatalf("osascript_exec rows = %d, want 1", got)
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
	r, pc, led, exec, _ := newReceiptRunner()
	in := ShortcutInput{Name: "Günlük Not"}
	if _, err := r.RunShortcut(context.Background(), "trace-1", "task-1", in); err != nil {
		t.Fatalf("first RunShortcut: %v", err)
	}
	out, err := r.RunShortcut(context.Background(), "trace-1", "task-1", in)
	if err != nil || !out.Replayed {
		t.Fatalf("second RunShortcut = (%+v, %v), want a replay", out, err)
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
