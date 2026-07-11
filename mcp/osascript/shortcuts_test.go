package osascript

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestRunShortcutEmptyNameRejectedBeforePolicy proves an empty name is a
// mechanical, pre-policy rejection (mirrors mcp/shell.HostExec's own
// "validate before any policy decision" ordering) — no Check, no exec.
func TestRunShortcutEmptyNameRejectedBeforePolicy(t *testing.T) {
	r, pc, _, exec := newTestRunner()

	_, err := r.RunShortcut(context.Background(), "trace-1", "task-1", ShortcutInput{Name: "   "})
	if err == nil {
		t.Fatal("err = nil, want the empty-name rejection")
	}
	if pc.checkCalls != 0 {
		t.Errorf("Policy.Check called %d times, want 0", pc.checkCalls)
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0", exec.callCount())
	}
}

// TestRunShortcutHappyPathArgvAndLedger proves shortcuts_run executes
// `shortcuts run <name> --input-path <canonical path>` and ledgers
// shortcuts_exec with trace_id.
func TestRunShortcutHappyPathArgvAndLedger(t *testing.T) {
	r, _, ledger, exec := newTestRunner()

	out, err := r.RunShortcut(context.Background(), "trace-5", "task-1", ShortcutInput{
		Name: "Yedekle", InputPath: "~/girdi.txt",
	})
	if err != nil {
		t.Fatalf("RunShortcut: %v", err)
	}
	if out.ExitCode != 0 {
		t.Errorf("out.ExitCode = %d, want 0", out.ExitCode)
	}

	call, ok := exec.lastCall()
	if !ok {
		t.Fatal("Executor.Run was never called")
	}
	if call.name != "shortcuts" {
		t.Errorf("exec name = %q, want shortcuts", call.name)
	}
	wantPrefix := []string{"run", "Yedekle", "--input-path"}
	if len(call.args) != 4 {
		t.Fatalf("exec args = %v, want 4 elements (run, name, --input-path, path)", call.args)
	}
	for i, a := range wantPrefix {
		if call.args[i] != a {
			t.Errorf("exec args[%d] = %q, want %q", i, call.args[i], a)
		}
	}
	if call.args[3] != "/Users/kahya/girdi.txt" {
		t.Errorf("exec args[3] (canonical input path) = %q, want %q", call.args[3], "/Users/kahya/girdi.txt")
	}

	evs := ledger.find("shortcuts_exec")
	if len(evs) != 1 {
		t.Fatalf("shortcuts_exec ledger events = %d, want 1", len(evs))
	}
	if evs[0].payload["trace_id"] != "trace-5" {
		t.Errorf("ledger payload trace_id = %v, want trace-5", evs[0].payload["trace_id"])
	}
	if evs[0].payload["shortcut_name"] != "Yedekle" {
		t.Errorf("ledger payload shortcut_name = %v, want Yedekle", evs[0].payload["shortcut_name"])
	}
}

// TestRunShortcutWithoutInputPath proves --input-path is omitted
// entirely (not passed as an empty string) when InputPath is unset.
func TestRunShortcutWithoutInputPath(t *testing.T) {
	r, _, _, exec := newTestRunner()

	_, err := r.RunShortcut(context.Background(), "trace-6", "task-1", ShortcutInput{Name: "Yedekle"})
	if err != nil {
		t.Fatalf("RunShortcut: %v", err)
	}
	call, ok := exec.lastCall()
	if !ok {
		t.Fatal("Executor.Run was never called")
	}
	if len(call.args) != 2 || call.args[0] != "run" || call.args[1] != "Yedekle" {
		t.Errorf("exec args = %v, want exactly [run, Yedekle]", call.args)
	}
}

// TestShortcutsRunApprovalToolInputContainsOnlyNameAndInputPath is this
// task's own acceptance criterion, verbatim: "shortcuts_run approval
// payload contains ONLY name + canonical input path and nothing else" —
// asserted here at the mcp/osascript tool_input envelope layer (the
// bytes actually hashed by Policy.Check/ConsumeToken); the SAME assertion
// against kahyad/internal/approval.BuildShortcut's own CanonicalBytes
// lives in kahyad/internal/approval/payload_test.go (that package is
// where the WYSIWYE approval CARD itself is rendered — this package
// cannot import it, see runner.go's own doc comment on the import
// boundary).
func TestShortcutsRunApprovalToolInputContainsOnlyNameAndInputPath(t *testing.T) {
	r, pc, _, _ := newTestRunner()

	_, err := r.RunShortcut(context.Background(), "trace-7", "task-1", ShortcutInput{
		Name: "Yedekle", InputPath: "~/girdi.txt",
	})
	if err != nil {
		t.Fatalf("RunShortcut: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(pc.lastCheckToolInput, &decoded); err != nil {
		t.Fatalf("tool_input is not valid JSON: %v (%q)", err, pc.lastCheckToolInput)
	}
	if len(decoded) != 2 {
		t.Fatalf("tool_input has %d top-level keys (%v), want exactly 2 (name, input_path)", len(decoded), decoded)
	}
	if _, ok := decoded["name"]; !ok {
		t.Error(`tool_input missing "name"`)
	}
	if _, ok := decoded["input_path"]; !ok {
		t.Error(`tool_input missing "input_path"`)
	}
	// Check and ConsumeToken must have hashed the IDENTICAL bytes.
	if string(pc.lastCheckToolInput) != string(pc.lastConsumeToolInput) {
		t.Errorf("Check tool_input (%q) != ConsumeToken tool_input (%q)", pc.lastCheckToolInput, pc.lastConsumeToolInput)
	}
}

// TestRunShortcutPolicyDeniedNeverExecutes mirrors the applescript_run/
// jxa_run deny-path test.
func TestRunShortcutPolicyDeniedNeverExecutes(t *testing.T) {
	r, pc, _, exec := newTestRunner()
	pc.decision = PolicyDecision{Result: PolicyResultDeny, Reason: "reddedildi"}

	_, err := r.RunShortcut(context.Background(), "trace-1", "task-1", ShortcutInput{Name: "Yedekle"})
	if err == nil {
		t.Fatal("err = nil, want the deny reason")
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0 on a deny", exec.callCount())
	}
}

// TestRunShortcutTimeoutKillsAndLedgers mirrors applescript_run's own
// timeout test — shortcuts_run shares the exact same hard 120s ceiling
// and osascript_timeout ledger event name.
func TestRunShortcutTimeoutKillsAndLedgers(t *testing.T) {
	r, _, ledger, exec := newTestRunner()
	exec.blockUntilCtxDone = true
	r.SetTimeoutUnit(time.Millisecond)

	out, err := r.RunShortcut(context.Background(), "trace-timeout", "task-1", ShortcutInput{Name: "Yedekle"})
	if err == nil {
		t.Fatal("err = nil, want the timeout error")
	}
	if !out.TimedOut {
		t.Error("out.TimedOut = false, want true")
	}
	evs := ledger.find("osascript_timeout")
	if len(evs) != 1 {
		t.Fatalf("osascript_timeout ledger events = %d, want 1", len(evs))
	}
	if evs[0].payload["tool"] != "shortcuts_run" {
		t.Errorf("ledger payload tool = %v, want shortcuts_run", evs[0].payload["tool"])
	}
}

// TestRunShortcutBadInputPathRejected proves an uncanonicalizable
// InputPath (e.g. a bidi/zero-width control rune mcp/fs.Canonicalize
// itself rejects) fails before any policy decision.
func TestRunShortcutBadInputPathRejected(t *testing.T) {
	r, pc, _, exec := newTestRunner()

	_, err := r.RunShortcut(context.Background(), "trace-1", "task-1", ShortcutInput{
		Name: "Yedekle", InputPath: "~/gir​di.txt",
	})
	if err == nil {
		t.Fatal("err = nil, want a canonicalization error")
	}
	if pc.checkCalls != 0 {
		t.Errorf("Policy.Check called %d times, want 0", pc.checkCalls)
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0", exec.callCount())
	}
}
