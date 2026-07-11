// live_test.go: an OPTIONAL, real end-to-end smoke test of the full gate
// chain PLUS the real osascript binary — gated behind
// KAHYA_OSASCRIPT_TESTS=1 (mirrors mcp/shell/container_test.go's own
// KAHYA_DOCKER_TESTS convention: SKIPPED when unset, so the suite stays
// green with no live confirmation; runs for REAL when set).
//
// This deliberately stays inside this task's own "no real app" boundary:
// the script below is a bare `return "..."` with no `tell application`
// at all, so it sends NO Apple event to any other process and needs NO
// TCC Automation grant — it only proves this package's OWN Runner+
// processGroupExecutor plumbing works against the real `osascript`
// binary, end to end, with an always-allow fake PolicyClient (this
// file's own scope is "does our process/stdin/exit-code handling work
// for real", not "is kahyad's real policy engine wired up", which is
// covered by kahyad/internal/policy's own test suite instead). The two
// acceptance criteria that DO need a real target app + a live TCC grant
// (a real Finder folder created under launchd, and the TCC checklist
// itself) are explicitly DEFERRED — see this task's own task file's
// Status note — and are NOT what this file exercises.
package osascript

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func requireOsascriptTests(t *testing.T) {
	t.Helper()
	if os.Getenv("KAHYA_OSASCRIPT_TESTS") != "1" {
		t.Skip("KAHYA_OSASCRIPT_TESTS not set — skipping the real-osascript smoke test")
	}
}

// TestLiveApplescriptRunNoTargetApp runs a real osascript process via the
// real Runner (fake policy/ledger, real Executor) with a script that
// touches no other app.
func TestLiveApplescriptRunNoTargetApp(t *testing.T) {
	requireOsascriptTests(t)

	r := NewRunner("/tmp", &fakePolicyClient{decision: allowDecision("tok-live")}, &fakeLedger{}, newFakeLogger())
	out, err := r.RunApplescript(context.Background(), "trace-live-as", "task-live", ScriptInput{
		Script: `return "kahya-osascript-live-test"`,
	})
	if err != nil {
		t.Fatalf("RunApplescript (live): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q, want 0", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "kahya-osascript-live-test") {
		t.Fatalf("Stdout = %q, want it to contain the returned string", out.Stdout)
	}
}

// TestLiveJXARunNoTargetApp is the JXA counterpart.
func TestLiveJXARunNoTargetApp(t *testing.T) {
	requireOsascriptTests(t)

	r := NewRunner("/tmp", &fakePolicyClient{decision: allowDecision("tok-live")}, &fakeLedger{}, newFakeLogger())
	out, err := r.RunJXA(context.Background(), "trace-live-jxa", "task-live", ScriptInput{
		Script: `"kahya-jxa-live-test"`,
	})
	if err != nil {
		t.Fatalf("RunJXA (live): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q, want 0", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "kahya-jxa-live-test") {
		t.Fatalf("Stdout = %q, want it to contain the returned string", out.Stdout)
	}
}

// TestLiveApplescriptRunRealTimeout proves the real 120s ceiling actually
// applies end to end (shrunk via SetTimeoutUnit so this test itself stays
// fast) against a script that hangs indefinitely with NO target app
// involved — `delay 30` merely sleeps inside osascript's own process,
// never sending any Apple event.
func TestLiveApplescriptRunRealTimeout(t *testing.T) {
	requireOsascriptTests(t)

	r := NewRunner("/tmp", &fakePolicyClient{decision: allowDecision("tok-live")}, &fakeLedger{}, newFakeLogger())
	r.SetTimeoutUnit(50 * time.Millisecond) // 120s ceiling -> 6s for this test
	start := time.Now()
	out, err := r.RunApplescript(context.Background(), "trace-live-timeout", "task-live", ScriptInput{
		Script: `delay 30
return "should never get here"`,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err = nil, want the timeout error")
	}
	if !out.TimedOut {
		t.Error("out.TimedOut = false, want true")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Run took %v, want it bounded well under the real 30s delay — process group not actually killed?", elapsed)
	}
}
