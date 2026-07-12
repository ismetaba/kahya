// engine_w403_test.go covers the W4-03 taint-check hook added to
// Engine.Check: resolve the session's tier FIRST (strictly before the
// ladder), deny any non-R class outright for a tainted session (reason
// tainted_session, rule RuleTaintedSessionV1), and let an R-class call
// (and a call carrying no SessionID at all) proceed through the ordinary
// ladder decision unaffected. These are the step-8 permanent regression
// tests HANDOFF §5 safety #2 requires.
package policy

import (
	"context"
	"testing"

	"kahya/kahyad/internal/taint"
)

// TestCheckTaintedSessionDeniesW1 is the step-8 test, verbatim: "tainted
// session W1 call => DENY tainted_session".
func TestCheckTaintedSessionDeniesW1(t *testing.T) {
	e, st := testEngine(t)
	tr := taint.New(st.Queries, st)
	ctx := context.Background()

	const sessionID = "session-tainted"
	if err := tr.Raise(ctx, "trace-raise", sessionID, "untrusted_output:web_fetch"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	e.SetTaintChecker(tr)

	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", SessionID: sessionID, TaskID: "t1", TraceID: "trace-w1",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny {
		t.Fatalf("Result = %q, want %q", d.Result, ResultDeny)
	}
	if d.Reason != ReasonTaintedSession {
		t.Fatalf("Reason = %q, want %q", d.Reason, ReasonTaintedSession)
	}
	if d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Rule = %q, want %q", d.Rule, RuleTaintedSessionV1)
	}
	if d.Token != "" {
		t.Fatalf("a tainted-session DENY must never carry a token, got %q", d.Token)
	}
}

// TestCheckTaintedSessionDeniesW3TooWithoutEverOfferingApproval proves the
// taint check runs BEFORE the class==W3 hardcoded-always-approval branch:
// a tainted session's W3 call is an outright DENY, never NEEDS_APPROVAL.
func TestCheckTaintedSessionDeniesW3TooWithoutEverOfferingApproval(t *testing.T) {
	e, st := testEngine(t)
	tr := taint.New(st.Queries, st)
	ctx := context.Background()

	const sessionID = "session-tainted-w3"
	if err := tr.Raise(ctx, "trace-raise", sessionID, "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	e.SetTaintChecker(tr)

	d, err := e.Check(ctx, CheckInput{
		Tool: "mail_send", SessionID: sessionID, TaskID: "t1", TraceID: "trace-w3",
		ToolInput: []byte(`{"to":"a@b.com"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny || d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Check(W3, tainted session) = %+v, want DENY/tainted_session", d)
	}
	if d.PendingApprovalID != "" {
		t.Fatalf("a tainted-session DENY must never mint a pending_approval_id, got %q", d.PendingApprovalID)
	}
}

// TestCheckTaintedSessionAllowsR is the step-8 test, verbatim: "R call =>
// allowed" - the taint check must never touch a class==R decision, even
// though the session it runs against is tainted. fs_read is pre-seeded to
// L1 (the level at which R auto-allows per the ladder table) so this test
// proves the UNDERLYING ladder decision (ALLOW) actually comes through
// unaltered, not merely that some non-DENY result appears.
func TestCheckTaintedSessionAllowsR(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_read", string(ClassR), "global", L1)

	tr := taint.New(st.Queries, st)
	ctx := context.Background()

	const sessionID = "session-tainted-r"
	if err := tr.Raise(ctx, "trace-raise", sessionID, "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	e.SetTaintChecker(tr)

	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_read", SessionID: sessionID, TaskID: "t1", TraceID: "trace-r",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result = %q, want %q (R must pass through a tainted session unaffected)", d.Result, ResultAllow)
	}
}

// TestCheckMissingSessionTaintRowFailsClosed proves the fail-closed
// default (kahyad/internal/taint.Tracker.Get: no row => tainted) actually
// reaches Check's own decision - an UNKNOWN session_id (never raised,
// never inserted clean) is denied a W1 call exactly like an explicitly
// tainted one.
func TestCheckMissingSessionTaintRowFailsClosed(t *testing.T) {
	e, st := testEngine(t)
	tr := taint.New(st.Queries, st)
	e.SetTaintChecker(tr)

	d, err := e.Check(context.Background(), CheckInput{
		Tool: "fs_write", SessionID: "session-never-seen", TaskID: "t1", TraceID: "trace-unknown",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny || d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Check(unknown session_id, W1) = %+v, want DENY/tainted_session", d)
	}
}

// TestCheckCleanSessionAllowsLadderThrough proves a CLEAN session's W1
// call is governed purely by the ordinary ladder (pre-seeded to L2, where
// W1 auto-allows) - the taint check must never itself deny a clean
// session.
func TestCheckCleanSessionAllowsLadderThrough(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	tr := taint.New(st.Queries, st)
	ctx := context.Background()
	const sessionID = "session-clean"
	if err := tr.InsertClean(ctx, "trace-clean", sessionID); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	e.SetTaintChecker(tr)

	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", SessionID: sessionID, TaskID: "t1", TraceID: "trace-clean-check",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result = %q, want %q (clean session's W1 must follow the ladder)", d.Result, ResultAllow)
	}
}

// TestCheckEmptySessionIDSkipsTaintCheck proves a caller that supplies no
// SessionID at all (mcp.go's policyGateMiddleware today - see this
// package's own SetTaintChecker doc comment on that documented scope
// boundary) still gets the ordinary ladder decision, never a taint-check
// DENY it never asked for.
func TestCheckEmptySessionIDSkipsTaintCheck(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)
	e.SetTaintChecker(taint.New(st.Queries, st))

	d, err := e.Check(context.Background(), CheckInput{
		Tool: "fs_write", TaskID: "t1", TraceID: "trace-no-session",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result = %q, want %q (no SessionID must skip the taint check)", d.Result, ResultAllow)
	}
}
