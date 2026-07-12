// engine_w403_test.go covers the W4-03 taint-check hook added to
// Engine.Check: resolve the session's tier FIRST (strictly before the
// ladder), deny any non-R class outright for a tainted session (reason
// tainted_session, rule RuleTaintedSessionV1), and let an R-class call
// proceed through the ordinary ladder decision unaffected. These are the
// step-8 permanent regression tests HANDOFF §5 safety #2 requires.
//
// Post-security-review (BLOCKER 1+2 fix): the taint check's session
// identity is now resolved SERVER-SIDE (policy.SessionResolver), from
// TraceID/TaskID, never trusted from the caller-supplied
// CheckInput.SessionID alone - see
// TestCheckServerResolvedTaintedSessionDeniesEvenWithEmptyWorkerSessionID,
// TestCheckMCPShapeNoTaskIDStillDeniesTaintedSession, and
// TestCheckUnresolvableSessionFailsClosedWhenResolverWired below. The
// original tests above (TestCheckTaintedSessionDeniesW1 and siblings, none
// of which wire a SessionResolver) continue to exercise the documented
// legacy in.SessionID-trusting fallback (SetSessionResolver's own doc
// comment) - proving that fallback still behaves exactly as it did before
// this fix, for any caller/test that predates it.
package policy

import (
	"context"
	"database/sql"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// seedTask inserts a minimal tasks row for the SessionResolver tests below
// (kahyad/internal/policy.StoreSessionResolver reads tasks.session_id by
// task_id/trace_id, mirroring what kahyad/internal/server's
// persistSessionStarted actually persists at session_started in
// production). sessionID may be "" to leave session_id NULL - the
// "session not started yet" case ResolveSession must treat as
// unresolved, not an error.
func seedTask(t *testing.T, st *store.Store, taskID, traceID, sessionID string) {
	t.Helper()
	_, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID:        taskID,
		TraceID:   traceID,
		SessionID: sql.NullString{String: sessionID, Valid: sessionID != ""},
		State:     "running",
		TaintTier: "untrusted",
		UpdatedAt: "2026-01-01T00:00:00Z",
		CreatedAt: "2026-01-01T00:00:00Z",
		Lane:      "normal",
	})
	if err != nil {
		t.Fatalf("seed task(id=%s, trace=%s): %v", taskID, traceID, err)
	}
}

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

// TestCheckServerResolvedTaintedSessionDeniesEvenWithEmptyWorkerSessionID
// replaces the now-incorrect (pre-fix) TestCheckEmptySessionIDSkipsTaintCheck:
// an empty CheckInput.SessionID must NOT bypass the taint check once a
// SessionResolver is wired (BLOCKER 1+2 fix) - the worker-supplied
// session_id was never trustworthy for this decision in the first place.
// Check must resolve the session SERVER-SIDE from TaskID/TraceID (the
// tasks row's own session_id, persisted by kahyad itself at
// session_started) and deny a tainted one exactly as it would if the
// caller HAD supplied that same session_id directly.
func TestCheckServerResolvedTaintedSessionDeniesEvenWithEmptyWorkerSessionID(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	const taskID, traceID, sessionID = "t-resolved", "trace-resolved", "session-server-resolved"
	seedTask(t, st, taskID, traceID, sessionID)

	tr := taint.New(st.Queries, st)
	ctx := context.Background()
	if err := tr.Raise(ctx, "trace-raise", sessionID, "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	e.SetTaintChecker(tr)
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))

	// SessionID deliberately left "" - simulating a worker that sends no
	// session_id at all (or a compromised one forging an empty/clean-
	// looking value): the resolver must find the REAL, tainted session
	// from taskID/traceID regardless.
	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", TaskID: taskID, TraceID: traceID, SessionID: "",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny || d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Check(resolved tainted session, empty worker SessionID) = %+v, want DENY/tainted_session", d)
	}
	if d.Reason != ReasonTaintedSession {
		t.Fatalf("Reason = %q, want %q", d.Reason, ReasonTaintedSession)
	}
}

// TestCheckMCPShapeNoTaskIDStillDeniesTaintedSession is the BLOCKER 2
// regression test: mcp.go's policyGateMiddleware builds a CheckInput
// carrying ONLY TraceID (no TaskID header today, no SessionID at all ever
// - see mcp.go's own doc comment). Before the fix, Check's
// `in.SessionID != ""` guard made this shape skip the taint check
// entirely on EVERY /v1/mcp call; it must now still resolve the session
// via TraceID alone (kahyad/internal/spawn sets the worker's
// KAHYA_TRACE_ID env to the task's own trace_id, so trace-only resolution
// is exactly what production needs) and deny a tainted one.
func TestCheckMCPShapeNoTaskIDStillDeniesTaintedSession(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	const taskID, traceID, sessionID = "t-mcp", "trace-mcp-shape", "session-mcp-tainted"
	seedTask(t, st, taskID, traceID, sessionID)

	tr := taint.New(st.Queries, st)
	ctx := context.Background()
	if err := tr.Raise(ctx, "trace-raise", sessionID, "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	e.SetTaintChecker(tr)
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))

	// Mirrors mcp.go's policyGateMiddleware's CheckInput exactly: TraceID
	// only, no TaskID, no SessionID.
	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", TraceID: traceID,
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny || d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Check(/v1/mcp-shape, tainted session) = %+v, want DENY/tainted_session", d)
	}
}

// TestCheckUnresolvableSessionFailsClosedWhenResolverWired is the BLOCKER 1
// fail-closed regression test: a SessionResolver IS wired, but the
// trace_id/task_id this Check call carries matches NO tasks row at all
// (never started, or a forged/unknown pair) - HANDOFF §5's verbatim rule
// ("kayıt yoksa oturum güvenilmez sayılır") means this must DENY exactly
// like an explicitly tainted session, never silently pass through to the
// ordinary ladder.
func TestCheckUnresolvableSessionFailsClosedWhenResolverWired(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	e.SetTaintChecker(taint.New(st.Queries, st))
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))

	d, err := e.Check(context.Background(), CheckInput{
		Tool: "fs_write", TaskID: "t-never-existed", TraceID: "trace-never-existed",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultDeny || d.Rule != RuleTaintedSessionV1 {
		t.Fatalf("Check(unresolvable trace/task, resolver wired) = %+v, want DENY/tainted_session", d)
	}
}

// TestCheckServerResolvedCleanSessionAllowsLadderThrough is the resolver
// path's positive control: wiring a SessionResolver must not itself deny
// anything - a task whose SERVER-persisted session is clean still gets
// the ordinary ladder decision, exactly like TestCheckCleanSessionAllowsLadderThrough
// above (which exercises the legacy in.SessionID-trusting fallback
// instead).
func TestCheckServerResolvedCleanSessionAllowsLadderThrough(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	const taskID, traceID, sessionID = "t-clean-resolved", "trace-clean-resolved", "session-clean-resolved"
	seedTask(t, st, taskID, traceID, sessionID)

	tr := taint.New(st.Queries, st)
	ctx := context.Background()
	if err := tr.InsertClean(ctx, "trace-clean", sessionID); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	e.SetTaintChecker(tr)
	e.SetSessionResolver(NewStoreSessionResolver(st.Queries))

	d, err := e.Check(ctx, CheckInput{
		Tool: "fs_write", TaskID: taskID, TraceID: traceID, SessionID: "",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Fatalf("Result = %q, want %q (server-resolved clean session must follow the ladder)", d.Result, ResultAllow)
	}
}
