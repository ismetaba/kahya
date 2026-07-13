package policy

// invariants_w7803_test.go holds the W78-03 §5-invariant gap-fill tests:
// the two invariant dimensions that had no permanent regression test of
// their own before this collection pass. They are ordinary (untagged)
// tests so they also run under `make test`; `make invariants` selects
// them via their pkg/TestName citations in docs/coverage.md.
//
//   - TestCheckContextTimeoutDeniesFailClosed  -> §4 IPC 5s-timeout flag /
//     Safety enforcement: POST /policy/check (Engine.Check) on a
//     deadline-exceeded / canceled context must return DENY, never a
//     permissive fallback. (The generic store-error dimension already has
//     TestDBErrorPathDeniesFailClosed; this adds the timeout dimension.)
//   - TestMemoryContentCannotLowerAPermission -> product principle D:
//     memory content can never lower a permission. The policy decision is
//     a pure function of tool metadata + the autonomy ladder + taint; no
//     fact/memory row can turn a gated decision into an ALLOW.

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
)

// TestCheckContextTimeoutDeniesFailClosed proves the §4 IPC contract's
// 5s-timeout flag at the decision layer: when the context handed to
// Engine.Check is already past its deadline (a timed-out or canceled
// /policy/check call), the state read fails and Check must fail CLOSED to
// DENY with ReasonPolicyStateError - even for a (tool,class,scope) triple
// seeded high enough on the ladder that a healthy call would auto-ALLOW.
func TestCheckContextTimeoutDeniesFailClosed(t *testing.T) {
	e, st := testEngine(t)
	// Seed fs_write/W1/global at L2 so a HEALTHY Check would auto-ALLOW -
	// the fail-closed DENY below is therefore load-bearing, not the
	// default gated outcome of an un-promoted tool.
	seedState(t, st, "fs_write", string(ClassW1), "global", L2)

	// Sanity: on a live context this triple auto-ALLOWs.
	if d, err := e.Check(context.Background(), CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-live", ToolInput: []byte(`{}`)}); err != nil || d.Result != ResultAllow {
		t.Fatalf("control Check = %+v (err=%v), want ALLOW so the timeout DENY below is meaningful", d, err)
	}

	// A context already past its deadline: the autonomy_state read inside
	// Check fails, and the decision must be DENY.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	d, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-timeout", ToolInput: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Check on an expired context: err = nil, want a context/deadline error surfaced")
	}
	if d.Result != ResultDeny {
		t.Fatalf("Result = %q, want %q (fail-closed on policy-check timeout)", d.Result, ResultDeny)
	}
	if d.Reason != ReasonPolicyStateError {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonPolicyStateError)
	}
}

// TestMemoryContentCannotLowerAPermission proves product principle D
// ("memory can never lower a permission"). We plant fact rows in the
// memory store that explicitly "assert" a blanket grant for the two
// tools, at the highest (user_edit) source tier and maximum confidence -
// exactly the shape an injection would want to forge - and prove the
// policy decision is byte-for-byte identical to the no-facts baseline:
//   - a W3 tool (mail_send) still returns NEEDS_APPROVAL, never ALLOW;
//   - an un-promoted W1 tool (fs_write at L0) still returns NEEDS_APPROVAL.
//
// The ladder/policy decides from static tool metadata + autonomy_state,
// never from anything in the facts table.
func TestMemoryContentCannotLowerAPermission(t *testing.T) {
	e, st := testEngine(t)
	ctx := context.Background()

	// Baseline decisions with an EMPTY facts table.
	baseW3, err := e.Check(ctx, CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-base-w3", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("baseline W3 Check: %v", err)
	}
	baseW1, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-base-w1", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("baseline W1 Check: %v", err)
	}
	if baseW3.Result != ResultNeedsApproval {
		t.Fatalf("baseline W3 = %q, want %q", baseW3.Result, ResultNeedsApproval)
	}
	if baseW1.Result != ResultNeedsApproval {
		t.Fatalf("baseline un-promoted W1 = %q, want %q", baseW1.Result, ResultNeedsApproval)
	}

	// Plant forged "grant" facts at the highest tier / max confidence.
	insertGrantFact(t, st, "mail_send", "policy_grant", "always_allow")
	insertGrantFact(t, st, "fs_write", "policy_grant", "always_allow")
	insertGrantFact(t, st, "autonomy", "level", "L4")

	// Post-injection decisions must be UNCHANGED - memory cannot lower a
	// permission.
	afterW3, err := e.Check(ctx, CheckInput{Tool: "mail_send", TaskID: "t1", TraceID: "trace-after-w3", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("post-injection W3 Check: %v", err)
	}
	afterW1, err := e.Check(ctx, CheckInput{Tool: "fs_write", TaskID: "t1", TraceID: "trace-after-w1", ToolInput: []byte(`{}`)})
	if err != nil {
		t.Fatalf("post-injection W1 Check: %v", err)
	}
	if afterW3.Result != baseW3.Result {
		t.Fatalf("W3 decision changed after planting grant facts: %q -> %q (memory must never lower a permission)", baseW3.Result, afterW3.Result)
	}
	if afterW3.Result == ResultAllow {
		t.Fatalf("W3 tool returned ALLOW after a planted grant fact - W3 must ALWAYS require written approval")
	}
	if afterW1.Result != baseW1.Result {
		t.Fatalf("un-promoted W1 decision changed after planting grant facts: %q -> %q", baseW1.Result, afterW1.Result)
	}
	if afterW1.Result == ResultAllow {
		t.Fatalf("un-promoted W1 tool returned ALLOW after a planted grant fact - memory must never raise autonomy")
	}
}

// insertGrantFact writes a single high-tier, max-confidence fact into the
// memory store's facts table. It is deliberately the most "authoritative"
// shape a memory row can take (user_edit tier, witnessed, large positive
// log-odds), so the test proves the policy engine ignores memory content
// regardless of how trusted that content claims to be.
func insertGrantFact(t *testing.T, st *store.Store, subject, predicate, object string) {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	if _, err := st.DB().Exec(
		`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, importance, status, updated_at, created_at)
		 VALUES (?, ?, ?, 'user_edit', 'witnessed', 99.0, 1.0, 'active', ?, ?)`,
		subject, predicate, object, now, now,
	); err != nil {
		t.Fatalf("insert grant fact (%s/%s/%s): %v", subject, predicate, object, err)
	}
}
