package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"kahya/kahyad/internal/policy"
)

// TestNewAnthproxyEgressGateHook_AttributesToTaskTraceID is the BLOCKER
// B/C regression test for the anthproxy path: kahyad/main.go builds this
// EXACT hook (via egress.NewAnthproxyEgressGateHook) and hands it to
// kahyad/internal/anthproxy.ProxyConfig.EgressGate, once per task. Before
// this fix, the hook's SessionInfo.SessionID was always "", so
// Gate.Check's sensitive-read check could never fire for a task's own
// model-call egress no matter what mcp/fs's fs_read seam had marked. This
// proves the hook now keys SessionID on the task's own trace_id — the
// SAME identity BLOCKER A's mcp/fs fix marks taint under.
func TestNewAnthproxyEgressGateHook_AttributesToTaskTraceID(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	sessions := NewSensitiveTracker()
	ledger := &fakeLedger{}
	gate := NewGate(cfg, sessions, newFakeBudget(), ledger, nil, nil)
	hook := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)

	traceID := "trace-e2e-taint"
	req := httptest.NewRequest(http.MethodPost, "http://ignored/v1/messages", nil)

	// Before any secret-lane read, this task's own model-call egress to
	// the (allowlisted) Anthropic upstream succeeds.
	if err := hook("task-1", traceID)(req); err != nil {
		t.Fatalf("pre-mark hook call: %v", err)
	}

	// BLOCKER A: mcp/fs's fs_read seam marks taint keyed on the REQUEST
	// trace_id (kahyad/internal/server.egressSensitiveMarker calls this
	// exact method, in production, on the SAME *Gate the hook consults).
	if err := gate.MarkSensitiveRead(context.Background(), traceID, traceID); err != nil {
		t.Fatalf("MarkSensitiveRead: %v", err)
	}

	// The hook itself still reaches the ALLOWLISTED upstream fine — the
	// sensitive block only hard-blocks allowlist-EXTERNAL egress, never
	// the model-call endpoint the task is SUPPOSED to keep using.
	if err := hook("task-1", traceID)(req); err != nil {
		t.Fatalf("post-mark hook call to the allowlisted upstream should still succeed: %v", err)
	}

	// BLOCKER B/C regression: a DIRECT Gate.Check for the SAME trace_id
	// (the identity the hook attributes every one of this task's requests
	// to) against an allowlist-EXTERNAL host is now hard-blocked — proving
	// the hook's SessionInfo{SessionID: traceID} really does reach the
	// SAME taint state fs_read's seam set, not a disconnected/always-empty
	// one.
	d, err := gate.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 10,
		SessionInfo{SessionID: traceID, TraceID: traceID})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Allow || d.Rule != EventBlockedSensitive {
		t.Fatalf("expected egress_blocked_sensitive for the SAME trace_id the anthproxy hook attributes to, got Allow=%v Rule=%q", d.Allow, d.Rule)
	}
}

// TestNewAnthproxyEgressGateHook_DifferentTaskUnaffected proves the hook
// never cross-taints: a DIFFERENT task's own trace_id, never marked
// sensitive, is unaffected by another task's mark.
func TestNewAnthproxyEgressGateHook_DifferentTaskUnaffected(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	sessions := NewSensitiveTracker()
	gate := NewGate(cfg, sessions, newFakeBudget(), &fakeLedger{}, nil, nil)
	hook := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)

	markedTrace := "trace-task-A-marked"
	otherTrace := "trace-task-B-unmarked"
	if err := gate.MarkSensitiveRead(context.Background(), markedTrace, markedTrace); err != nil {
		t.Fatalf("MarkSensitiveRead: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://ignored/v1/messages", nil)
	if err := hook("task-B", otherTrace)(req); err != nil {
		t.Fatalf("task B's own (unmarked) hook call to the allowlisted upstream: %v", err)
	}
	d, err := gate.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 10,
		SessionInfo{SessionID: otherTrace, TraceID: otherTrace})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// evil.example.com is denied regardless (it is never allowlisted) —
	// the point is WHICH reason: the ORDINARY allowlist miss, never the
	// sensitive-taint one task A's mark produced, proving task B's own
	// trace_id never inherited it.
	if d.Allow || d.Rule != EventBlockedAllowlist {
		t.Fatalf("expected egress_blocked_allowlist (task B's trace_id must not inherit task A's taint), got Allow=%v Rule=%q Reason=%q", d.Allow, d.Rule, d.Reason)
	}
}
