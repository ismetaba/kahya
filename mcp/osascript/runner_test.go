package osascript

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestRunner() (*Runner, *fakePolicyClient, *fakeLedger, *fakeExecutor) {
	pc := &fakePolicyClient{decision: allowDecision("tok-1")}
	ledger := &fakeLedger{}
	exec := &fakeExecutor{result: Result{ExitCode: 0, Stdout: []byte("ok")}}
	r := &Runner{
		Home: "/Users/kahya", Policy: pc, Ledger: ledger, Log: newFakeLogger(),
		Exec: exec, timeoutUnit: time.Millisecond, // shrink the 120s ceiling to 120ms for fast tests
	}
	return r, pc, ledger, exec
}

// ---- scan-rejection: mechanical, pre-policy, no execution ----

// TestRunApplescriptScanRejectedNeverConsultsPolicy proves a shell-shaped
// body is rejected BEFORE any policy decision (mirrors mcp/fs's/mcp/
// shell's "deny-glob/workdir-scope check runs BEFORE approval flow"
// ordering) — no Check, no ConsumeToken, no exec, and the rejection is
// returned as a non-error, Rejected:true output (never a Go error) so a
// Reroute suggestion, when present, is actually visible to the caller
// (the MCP SDK drops Out entirely on err != nil).
func TestRunApplescriptScanRejectedNeverConsultsPolicy(t *testing.T) {
	r, pc, ledger, exec := newTestRunner()

	out, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{
		Script: `do shell script "whoami"`,
	})
	if err != nil {
		t.Fatalf("RunApplescript = err %v, want nil (rejection travels via Rejected:true output)", err)
	}
	if !out.Rejected {
		t.Fatalf("out.Rejected = false, want true")
	}
	if out.Reason != reasonShellShaped {
		t.Errorf("out.Reason = %q, want %q", out.Reason, reasonShellShaped)
	}
	if out.Reroute == nil || out.Reroute.Tool != "shell_docker" || out.Reroute.Command != "whoami" {
		t.Errorf("out.Reroute = %+v, want a shell_docker suggestion with command %q", out.Reroute, "whoami")
	}
	if pc.checkCalls != 0 {
		t.Errorf("Policy.Check called %d times, want 0 (scan rejects before any policy decision)", pc.checkCalls)
	}
	if pc.consumeCalls != 0 {
		t.Errorf("Policy.ConsumeToken called %d times, want 0", pc.consumeCalls)
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0 — nothing may execute on a scan rejection", exec.callCount())
	}
	if evs := ledger.find("osascript_scan_rejected"); len(evs) != 1 {
		t.Errorf("osascript_scan_rejected ledger events = %d, want 1", len(evs))
	} else if evs[0].traceID != "trace-1" {
		t.Errorf("ledger traceID = %q, want %q", evs[0].traceID, "trace-1")
	}
}

// TestRunJXAScanRejectedNSTaskNeverExecutes covers jxa_run's own scan
// path (NSTask via the ObjC bridge) with the identical "never executes"
// assertion.
func TestRunJXAScanRejectedNSTaskNeverExecutes(t *testing.T) {
	r, _, _, exec := newTestRunner()

	out, err := r.RunJXA(context.Background(), "trace-1", "task-1", ScriptInput{
		Script: `ObjC.import('Foundation'); $.NSTask.alloc().init();`,
	})
	if err != nil {
		t.Fatalf("RunJXA = err %v, want nil", err)
	}
	if !out.Rejected {
		t.Fatal("out.Rejected = false, want true")
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0", exec.callCount())
	}
}

// ---- policy deny/needs-approval: no execution ----

func TestRunApplescriptPolicyDeniedNeverExecutes(t *testing.T) {
	r, pc, _, exec := newTestRunner()
	pc.decision = PolicyDecision{Result: PolicyResultDeny, Reason: "reddedildi"}

	_, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{
		Script: `tell application "Finder" to get name of every window`,
	})
	if err == nil || !strings.Contains(err.Error(), "reddedildi") {
		t.Fatalf("err = %v, want it to contain the deny reason", err)
	}
	if pc.consumeCalls != 0 {
		t.Errorf("ConsumeToken called %d times, want 0 on a deny", pc.consumeCalls)
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0 on a deny", exec.callCount())
	}
}

func TestRunApplescriptNeedsApprovalNeverExecutes(t *testing.T) {
	r, pc, _, exec := newTestRunner()
	pc.decision = PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "onay gerekiyor", PendingApprovalID: "p1"}

	_, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{
		Script: `tell application "Finder" to get name of every window`,
	})
	if err == nil {
		t.Fatal("err = nil, want the needs-approval reason surfaced as an error")
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0 while approval is pending", exec.callCount())
	}
}

// TestRunApplescriptConsumeTokenFailureNeverExecutes proves that when
// ConsumeToken rejects (the real Engine's own token_verify_failed path —
// see kahyad/internal/policy/engine_w309_test.go's byte-mutation
// regression test for where that hash comparison is actually enforced),
// this package's own Runner surfaces the failure and NEVER reaches
// Exec.Run — the gate chain's "consume before execute" ordering holds
// even when Check itself already said Allow.
func TestRunApplescriptConsumeTokenFailureNeverExecutes(t *testing.T) {
	r, pc, _, exec := newTestRunner()
	pc.consumeErr = errors.New("policy: approval token invalid, expired, or already consumed")

	_, err := r.RunApplescript(context.Background(), "trace-1", "task-1", ScriptInput{
		Script: `tell application "Finder" to get name of every window`,
	})
	if err == nil {
		t.Fatal("err = nil, want the ConsumeToken failure surfaced")
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times, want 0 when ConsumeToken fails", exec.callCount())
	}
}

// ---- happy path: exact stdin/argv, ledger, trace_id ----

// TestRunApplescriptHappyPathUsesStdinNeverArgv proves applescript_run
// executes `osascript -` with the script bytes on STDIN, verbatim — never
// as an argv element (this task's own spec: "NEVER argv").
func TestRunApplescriptHappyPathUsesStdinNeverArgv(t *testing.T) {
	r, pc, ledger, exec := newTestRunner()
	script := `tell application "Finder" to make new folder at desktop`

	out, err := r.RunApplescript(context.Background(), "trace-42", "task-7", ScriptInput{
		Script: script, TargetApp: "Finder",
	})
	if err != nil {
		t.Fatalf("RunApplescript: %v", err)
	}
	if out.ExitCode != 0 || out.Stdout != "ok" {
		t.Errorf("out = %+v, want the fake executor's canned result", out)
	}

	call, ok := exec.lastCall()
	if !ok {
		t.Fatal("Executor.Run was never called")
	}
	if call.name != "osascript" {
		t.Errorf("exec name = %q, want osascript", call.name)
	}
	if len(call.args) != 1 || call.args[0] != "-" {
		t.Errorf("exec args = %v, want [\"-\"]", call.args)
	}
	if string(call.stdin) != script {
		t.Errorf("exec stdin = %q, want the script bytes verbatim: %q", call.stdin, script)
	}
	for _, a := range call.args {
		if strings.Contains(a, "Finder") || strings.Contains(a, "desktop") {
			t.Errorf("script content leaked into argv: %v", call.args)
		}
	}

	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Errorf("checkCalls=%d consumeCalls=%d, want 1 and 1", pc.checkCalls, pc.consumeCalls)
	}
	// The EXACT bytes Check saw and ConsumeToken saw must be identical —
	// this package's own half of the WYSIWYE invariant (no re-derivation
	// between the two calls that could ever drift).
	if string(pc.lastCheckToolInput) != string(pc.lastConsumeToolInput) {
		t.Errorf("Check toolInput (%q) != ConsumeToken toolInput (%q)", pc.lastCheckToolInput, pc.lastConsumeToolInput)
	}
	if !strings.Contains(string(pc.lastCheckToolInput), "make new folder at desktop") {
		t.Errorf("tool_input %q does not contain the script bytes", pc.lastCheckToolInput)
	}

	evs := ledger.find("osascript_exec")
	if len(evs) != 1 {
		t.Fatalf("osascript_exec ledger events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.traceID != "trace-42" {
		t.Errorf("ledger traceID = %q, want trace-42", ev.traceID)
	}
	if ev.payload["trace_id"] != "trace-42" {
		t.Errorf("ledger payload trace_id = %v, want trace-42", ev.payload["trace_id"])
	}
	if ev.payload["lang"] != "applescript" {
		t.Errorf("ledger payload lang = %v, want applescript", ev.payload["lang"])
	}
	if ev.payload["target_app"] != "Finder" {
		t.Errorf("ledger payload target_app = %v, want Finder", ev.payload["target_app"])
	}
}

// TestRunJXAHappyPathUsesJavaScriptFlag proves jxa_run executes
// `osascript -l JavaScript -`.
func TestRunJXAHappyPathUsesJavaScriptFlag(t *testing.T) {
	r, _, ledger, exec := newTestRunner()

	_, err := r.RunJXA(context.Background(), "trace-9", "task-1", ScriptInput{
		Script: `Application("Finder").windows().length;`, TargetApp: "Finder",
	})
	if err != nil {
		t.Fatalf("RunJXA: %v", err)
	}

	call, ok := exec.lastCall()
	if !ok {
		t.Fatal("Executor.Run was never called")
	}
	wantArgs := []string{"-l", "JavaScript", "-"}
	if len(call.args) != len(wantArgs) {
		t.Fatalf("exec args = %v, want %v", call.args, wantArgs)
	}
	for i, a := range wantArgs {
		if call.args[i] != a {
			t.Errorf("exec args[%d] = %q, want %q", i, call.args[i], a)
		}
	}
	evs := ledger.find("osascript_exec")
	if len(evs) != 1 || evs[0].payload["lang"] != "jxa" {
		t.Fatalf("osascript_exec ledger = %+v, want exactly one row with lang=jxa", evs)
	}
}

// ---- timeout: process group killed, osascript_timeout ledgered ----

// TestRunApplescriptTimeoutKillsAndLedgers is this task's spec's own
// acceptance criterion: a stub executor that sleeps (here: blocks until
// ctx.Done()) causes the hard timeout to fire, and an osascript_timeout
// ledger event with trace_id is recorded. The REAL process-group kill
// mechanics (exec.go's killProcessGroup) are exercised separately in
// exec_test.go against a real subprocess — this test proves Runner.run's
// OWN handling of a timed-out Executor, decoupled from any real process.
func TestRunApplescriptTimeoutKillsAndLedgers(t *testing.T) {
	r, _, ledger, exec := newTestRunner()
	exec.blockUntilCtxDone = true
	r.SetTimeoutUnit(time.Millisecond) // 120s -> 120ms for this test

	start := time.Now()
	out, err := r.RunApplescript(context.Background(), "trace-timeout", "task-1", ScriptInput{
		Script: `tell application "Finder" to get name of every window`,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("err = nil, want the timeout error")
	}
	if !out.TimedOut {
		t.Errorf("out.TimedOut = false, want true")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, want it bounded by the (shrunk) 120-unit ceiling", elapsed)
	}
	evs := ledger.find("osascript_timeout")
	if len(evs) != 1 {
		t.Fatalf("osascript_timeout ledger events = %d, want 1", len(evs))
	}
	if evs[0].payload["trace_id"] != "trace-timeout" {
		t.Errorf("ledger payload trace_id = %v, want trace-timeout", evs[0].payload["trace_id"])
	}
}

// ---- TCC Automation-denied ----

// TestRunApplescriptAutomationDeniedReturnsTypedError proves the -1743/
// errAEEventNotPermitted stderr marker becomes an *AutomationDeniedError
// (errors.As-able, mirrors mcp/fs.FullDiskAccessError's pattern) carrying
// this task's spec's own verbatim Turkish message.
func TestRunApplescriptAutomationDeniedReturnsTypedError(t *testing.T) {
	r, _, ledger, _ := newTestRunner()
	r.Exec = &fakeExecutor{result: Result{
		ExitCode: 1, Stderr: []byte("execution error: Finder got an error: Not authorized to send Apple events to Finder. (-1743)"),
	}}

	_, err := r.RunApplescript(context.Background(), "trace-tcc", "task-1", ScriptInput{
		Script: `tell application "Finder" to get name of every window`, TargetApp: "Finder",
	})
	var tccErr *AutomationDeniedError
	if !errors.As(err, &tccErr) {
		t.Fatalf("err = %v (%T), want *AutomationDeniedError", err, err)
	}
	if tccErr.App != "Finder" {
		t.Errorf("AutomationDeniedError.App = %q, want Finder", tccErr.App)
	}
	want := "Otomasyon izni gerekli: Finder — docs/tcc-automation.md adımlarını izleyin"
	if err.Error() != want {
		t.Errorf("err.Error() = %q, want %q", err.Error(), want)
	}
	if evs := ledger.find("osascript_automation_denied"); len(evs) != 1 {
		t.Errorf("osascript_automation_denied ledger events = %d, want 1", len(evs))
	}
}

// TestAutomationDeniedIgnoresCleanExit proves exit code 0 never trips the
// TCC check, even if stderr happened to contain the marker text (e.g. as
// part of ordinary output) — automationDenied only ever applies to a
// non-zero exit.
func TestAutomationDeniedIgnoresCleanExit(t *testing.T) {
	if automationDenied(0, []byte("-1743 mentioned but exit was clean")) {
		t.Fatal("automationDenied(0, ...) = true, want false")
	}
}
