// taint_gate_test.go is W5-05's own phase-gate test (HANDOFF §6 W5
// acceptance, verbatim): "tainted brifing oturumundan doğrudan W-araç
// çağrısı reddediliyor (aynı eylem temiz oturumdan geçiyor)" - the SAME
// tool AND the SAME target (byte-identical ToolInput) is DENIED from a
// tainted/untrusted session (RuleTaintedSessionV1, exactly like a real
// briefing session per kahyad/internal/briefing's own
// TestRunRegistersUntrustedTaintBeforeSpawn) and reaches kahyad's normal
// approval flow (ResultNeedsApproval, a minted PendingApprovalID - the
// WYSIWYE approval pipeline W3-06 renders from) from an otherwise-identical
// CLEAN session. Only the session's own taint tier differs between the two
// Check calls below - never the tool, never the input bytes, never the
// ladder state - so this test cannot be gamed by silently substituting a
// different action for the "clean" half.
//
// mail_send (ClassW3) is used deliberately: W3 NEVER auto-allows at any
// ladder level (engine.go's own hard-coded rule, TestW3NeverAllowsEvenWithForgedL4Row),
// so the clean-session branch is guaranteed to land on ResultNeedsApproval
// - the actual "approval flow" HANDOFF §6 W5's acceptance sentence means -
// rather than sometimes auto-allowing depending on incidental ladder state.
package policy

import (
	"context"
	"testing"

	"kahya/kahyad/internal/taint"
)

// TestSameToolSameTargetDeniedTaintedAllowedClean is THE W5-05 gate test.
func TestSameToolSameTargetDeniedTaintedAllowedClean(t *testing.T) {
	e, st := testEngine(t)
	tr := taint.New(st.Queries, st)
	e.SetTaintChecker(tr)
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))
	ctx := context.Background()

	// The IDENTICAL (tool, target) pair both Check calls below use -
	// mail_send with byte-for-byte the same recipient/body. Constructed
	// ONCE and reused for both calls so there is no possibility of the two
	// branches silently drifting into two different actions.
	const tool = "mail_send"
	toolInput := []byte(`{"to":"muhasebe@example.com","subject":"Fatura","body":"Ekli faturayı onaylıyorum."}`)

	// --- Branch 1: the briefing/untrusted session (tainted BY DESIGN, the
	// exact same InsertUntrusted call kahyad/internal/briefing.Orchestrator.
	// Run makes BEFORE it ever spawns the worker). ---
	const briefingTaskID, briefingTraceID, briefingSessionID = "t-briefing", "trace-briefing", "session-briefing-untrusted"
	seedTask(t, st, briefingTaskID, briefingTraceID, briefingSessionID)
	if err := tr.InsertUntrusted(ctx, briefingTraceID, briefingSessionID, "briefing:untrusted_by_design"); err != nil {
		t.Fatalf("InsertUntrusted (briefing session): %v", err)
	}

	taintedDecision, err := e.Check(ctx, CheckInput{
		Tool: tool, TaskID: briefingTaskID, TraceID: briefingTraceID,
		ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check (tainted/briefing session): %v", err)
	}
	if taintedDecision.Result != ResultDeny {
		t.Fatalf("tainted-session Check.Result = %q, want %q", taintedDecision.Result, ResultDeny)
	}
	if taintedDecision.Rule != RuleTaintedSessionV1 {
		t.Fatalf("tainted-session Check.Rule = %q, want %q", taintedDecision.Rule, RuleTaintedSessionV1)
	}
	if taintedDecision.Reason != ReasonTaintedSession {
		t.Fatalf("tainted-session Check.Reason = %q, want %q", taintedDecision.Reason, ReasonTaintedSession)
	}
	if taintedDecision.PendingApprovalID != "" {
		t.Fatalf("tainted-session DENY must never mint a pending_approval_id, got %q", taintedDecision.PendingApprovalID)
	}
	if taintedDecision.Token != "" {
		t.Fatalf("tainted-session DENY must never mint a token, got %q", taintedDecision.Token)
	}

	// --- Branch 2: a fresh, otherwise-identical CLEAN session - same tool,
	// byte-identical ToolInput, only the session's own taint tier differs. ---
	const cleanTaskID, cleanTraceID, cleanSessionID = "t-clean", "trace-clean", "session-clean-control"
	seedTask(t, st, cleanTaskID, cleanTraceID, cleanSessionID)
	if err := tr.InsertClean(ctx, cleanTraceID, cleanSessionID); err != nil {
		t.Fatalf("InsertClean (control session): %v", err)
	}

	cleanDecision, err := e.Check(ctx, CheckInput{
		Tool: tool, TaskID: cleanTaskID, TraceID: cleanTraceID,
		ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check (clean session): %v", err)
	}
	if cleanDecision.Result != ResultNeedsApproval {
		t.Fatalf("clean-session Check.Result = %q, want %q (the approval flow) - got reason=%q rule=%q",
			cleanDecision.Result, ResultNeedsApproval, cleanDecision.Reason, cleanDecision.Rule)
	}
	if cleanDecision.PendingApprovalID == "" {
		t.Fatal("clean-session NEEDS_APPROVAL must mint a pending_approval_id, got empty")
	}
	if cleanDecision.Rule != RuleLadderV1 {
		t.Fatalf("clean-session Check.Rule = %q, want %q (ordinary ladder decision, not the taint rule)", cleanDecision.Rule, RuleLadderV1)
	}
}

// TestSameToolSameTargetTaintedDenyEvenWhenLadderWouldAutoAllow proves the
// taint DENY is not merely "the approval flow was going to be reached
// anyway" - it fires even for a W1 tool pre-seeded to auto-allow (L2),
// where the clean-session control genuinely resolves to ResultAllow
// (never approval, never deny) - the sharpest possible contrast for the
// SAME (tool, target) pair.
func TestSameToolSameTargetTaintedDenyEvenWhenLadderWouldAutoAllow(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2) // W1 auto-allows at/above L2

	tr := taint.New(st.Queries, st)
	e.SetTaintChecker(tr)
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))
	ctx := context.Background()

	const tool = "fs_write"
	toolInput := []byte(`{"path":"/Users/x/Documents/notlar.md","content":"güncelleme"}`)

	const taintedTaskID, taintedTraceID, taintedSessionID = "t-briefing-2", "trace-briefing-2", "session-briefing-untrusted-2"
	seedTask(t, st, taintedTaskID, taintedTraceID, taintedSessionID)
	if err := tr.InsertUntrusted(ctx, taintedTraceID, taintedSessionID, "briefing:untrusted_by_design"); err != nil {
		t.Fatalf("InsertUntrusted: %v", err)
	}
	taintedDecision, err := e.Check(ctx, CheckInput{Tool: tool, TaskID: taintedTaskID, TraceID: taintedTraceID, ToolInput: toolInput})
	if err != nil {
		t.Fatalf("Check (tainted): %v", err)
	}
	if taintedDecision.Result != ResultDeny || taintedDecision.Rule != RuleTaintedSessionV1 {
		t.Fatalf("tainted-session Check = %+v, want DENY/tainted_session", taintedDecision)
	}

	const cleanTaskID, cleanTraceID, cleanSessionID = "t-clean-2", "trace-clean-2", "session-clean-control-2"
	seedTask(t, st, cleanTaskID, cleanTraceID, cleanSessionID)
	if err := tr.InsertClean(ctx, cleanTraceID, cleanSessionID); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	cleanDecision, err := e.Check(ctx, CheckInput{Tool: tool, TaskID: cleanTaskID, TraceID: cleanTraceID, ToolInput: toolInput})
	if err != nil {
		t.Fatalf("Check (clean): %v", err)
	}
	if cleanDecision.Result != ResultAllow {
		t.Fatalf("clean-session Check.Result = %q, want %q (ladder auto-allows W1 at L2)", cleanDecision.Result, ResultAllow)
	}
	if cleanDecision.Token == "" {
		t.Error("clean-session ResultAllow for a W1 tool should carry an undo-window token, got empty")
	}
}
