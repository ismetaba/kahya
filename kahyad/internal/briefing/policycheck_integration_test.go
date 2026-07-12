// policycheck_integration_test.go is this task's own integration
// acceptance criterion: a W1 tool call from the briefing session gets
// DENY from kahyad/internal/policy.Engine.Check (untrusted tier => R-only)
// and the denial is ledgered. It exercises a REAL store.Store, a REAL
// taint.Tracker, and a REAL policy.Engine wired exactly the way main.go
// wires production (SetTaintChecker + SetSessionResolver) - the SAME
// mechanism that answers POST /policy/check for every other tool call in
// this codebase, proving the briefing session is denied by the ordinary
// production enforcement path, not by anything this package does itself.
package briefing

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
)

// TestBriefingSessionW1ToolCallDeniedByPolicyEngine runs one full
// Orchestrator.Run (real store, real taint.Tracker, real TaskStore write),
// then issues a W1 tool call (fs_write) against a real policy.Engine for
// the EXACT (task_id, trace_id) that run produced, asserting DENY +
// RuleTaintedSessionV1 + a ledgered policy_decision row - HANDOFF §5
// safety #2's "untrusted tier = R-tools + notify only", enforced end to
// end.
func TestBriefingSessionW1ToolCallDeniedByPolicyEngine(t *testing.T) {
	st := newTestStore(t)
	tr := taint.New(st.Queries, st)

	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}

	o := &Orchestrator{
		Classifier: classifier,
		Spawner:    spawner,
		Delivery:   delivery,
		Taint:      tr,
		TaskStore:  st.Queries,
		Ledger:     st,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	const traceID = "trace-briefing-policycheck"
	result, err := o.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true")
	}
	if result.TaskID == "" || result.SessionID == "" {
		t.Fatalf("Result = %+v, want non-empty TaskID/SessionID", result)
	}

	pol := policy.Policy{
		Tools: []policy.ToolRule{
			{Name: "fs_write", Class: policy.ClassW1, ScopeKey: "global"},
			{Name: "fs_read", Class: policy.ClassR, ScopeKey: "global"},
		},
		ToolsByName: map[string]policy.ToolRule{
			"fs_write": {Name: "fs_write", Class: policy.ClassW1, ScopeKey: "global"},
			"fs_read":  {Name: "fs_read", Class: policy.ClassR, ScopeKey: "global"},
		},
	}
	engine := policy.NewEngine(pol, st.Queries, st)
	engine.SetTaintChecker(tr)
	engine.SetSessionResolver(policy.NewStoreSessionResolver(st.Queries))

	beforeDecisions := countStoreEventsOfKind(t, st, "policy_decision")

	d, err := engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: result.TaskID, TraceID: traceID,
		ToolInput: []byte(`{"path":"/tmp/whatever"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != policy.ResultDeny {
		t.Fatalf("Check(fs_write, briefing session) Result = %q, want %q", d.Result, policy.ResultDeny)
	}
	if d.Rule != policy.RuleTaintedSessionV1 {
		t.Fatalf("Check(fs_write, briefing session) Rule = %q, want %q", d.Rule, policy.RuleTaintedSessionV1)
	}
	if d.Reason != policy.ReasonTaintedSession {
		t.Errorf("Reason = %q, want %q", d.Reason, policy.ReasonTaintedSession)
	}
	if d.Token != "" {
		t.Errorf("a tainted-session DENY must never carry a token, got %q", d.Token)
	}

	afterDecisions := countStoreEventsOfKind(t, st, "policy_decision")
	if afterDecisions != beforeDecisions+1 {
		t.Fatalf("policy_decision events = %d, want exactly %d more (the denial must be ledgered)", afterDecisions, beforeDecisions+1)
	}

	// Positive control: an R-class call from the SAME session still
	// passes through the ordinary ladder (untrusted tier is R + notify,
	// never R-denied) - proving the taint check specifically targets
	// non-R classes, not a blanket deny of the whole session.
	rd, err := engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_read", TaskID: result.TaskID, TraceID: traceID,
		ToolInput: []byte(`{"path":"/tmp/whatever"}`),
	})
	if err != nil {
		t.Fatalf("Check(fs_read): %v", err)
	}
	if rd.Result == policy.ResultDeny && rd.Rule == policy.RuleTaintedSessionV1 {
		t.Fatalf("Check(fs_read, briefing session) was denied by the taint check - untrusted tier must still allow R through to the ordinary ladder: %+v", rd)
	}
}

// countStoreEventsOfKind mirrors kahyad/internal/policy/engine_test.go's
// own countEvents helper (duplicated here rather than imported - it is
// unexported in that package).
func countStoreEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}
