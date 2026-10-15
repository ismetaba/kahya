package egress

import (
	"bytes"
	"context"
	"io"
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

// TestNewAnthproxyEgressGateHook_DenialCarriesTerminalReason (FIX-U-4,
// FIX-FINAL u4-allowlist-sensitive-403-not-terminal): the hook's deny is a
// *GateDenial whose Error() is the decision's Turkish Reason byte-for-byte
// (the worker's 403 body is unchanged). An allowlist or sensitive-read
// denial carries TerminalEgressReason "egress_blocked" (deterministic: the
// model-call target is fixed); a budget denial carries "" (daily, not
// terminal).
func TestNewAnthproxyEgressGateHook_DenialCarriesTerminalReason(t *testing.T) {
	req := func(n int64) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://ignored/v1/messages", nil)
		r.ContentLength = n
		return r
	}
	check := func(t *testing.T, err error, wantRule, wantTerminal string, ledger *fakeLedger) {
		t.Helper()
		d, ok := err.(*GateDenial)
		if !ok {
			t.Fatalf("hook error = %T %v, want *GateDenial", err, err)
		}
		if d.Rule != wantRule || d.TerminalEgressReason() != wantTerminal {
			t.Fatalf("denial = %+v terminal=%q, want rule %q terminal %q", d, d.TerminalEgressReason(), wantRule, wantTerminal)
		}
		if d.Error() != d.Reason || d.Reason == "" {
			t.Fatalf("Error() = %q, want the decision Reason %q byte-for-byte", d.Error(), d.Reason)
		}
	}

	t.Run("allowlist", func(t *testing.T) {
		cfg := policy.EgressConfig{Allowlist: []policy.EgressAllowEntry{{Host: "other.example"}}, DefaultDailyByteBudget: 1 << 20}
		gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)
		err := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)("t1", "tr1")(req(10))
		check(t, err, EventBlockedAllowlist, TerminalEgressReason, nil)
	})
	t.Run("sensitive", func(t *testing.T) {
		cfg := policy.EgressConfig{Allowlist: []policy.EgressAllowEntry{{Host: "other.example"}}, DefaultDailyByteBudget: 1 << 20}
		sessions := NewSensitiveTracker()
		gate := NewGate(cfg, sessions, newFakeBudget(), &fakeLedger{}, nil, nil)
		if err := gate.MarkSensitiveRead(context.Background(), "tr1", "tr1"); err != nil {
			t.Fatalf("MarkSensitiveRead: %v", err)
		}
		err := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)("t1", "tr1")(req(10))
		check(t, err, EventBlockedSensitive, TerminalEgressReason, nil)
	})
	t.Run("budget", func(t *testing.T) {
		cfg := policy.EgressConfig{Allowlist: []policy.EgressAllowEntry{{Host: "api.anthropic.com"}}, DefaultDailyByteBudget: 100}
		gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)
		err := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)("t1", "tr1")(req(1000))
		check(t, err, EventBlockedBudget, "", nil)
	})
	if TerminalEgressReason != "egress_blocked" {
		t.Fatalf("TerminalEgressReason = %q, must equal anthproxy.ReasonEgressBlocked %q", TerminalEgressReason, "egress_blocked")
	}
}

// TestNewAnthproxyEgressGateHook_ChunkedBodyCountsRealBytes (FIX-FINAL
// egress-budget-chunked-bypass): a body of unknown length (chunked,
// ContentLength -1) used to be admitted as 0 bytes, so the per-host daily
// byte budget never moved. It is now sized by its real byte count, its
// bytes are restored unchanged for the proxy to forward, and an oversized
// one is refused.
func TestNewAnthproxyEgressGateHook_ChunkedBodyCountsRealBytes(t *testing.T) {
	budget := newFakeBudget()
	gate := NewGate(policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1000,
	}, NewSensitiveTracker(), budget, &fakeLedger{}, nil, nil)
	hook := NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)("task-1", "trace-1")
	chunked := func(body []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://ignored/v1/messages", io.NopCloser(bytes.NewReader(body)))
		r.ContentLength = -1
		return r
	}
	recorded := func() int64 {
		var total int64
		for _, v := range budget.m {
			total += v
		}
		return total
	}

	body := bytes.Repeat([]byte("a"), 600)
	r := chunked(body)
	if err := hook(r); err != nil {
		t.Fatalf("first 600-byte chunked call refused: %v", err)
	}
	if got := recorded(); got != 600 {
		t.Fatalf("budget recorded %d bytes, want 600", got)
	}
	forwarded, err := io.ReadAll(r.Body)
	if err != nil || !bytes.Equal(forwarded, body) {
		t.Fatalf("restored body = %d bytes (err %v), want the original %d bytes unchanged", len(forwarded), err, len(body))
	}
	if r.ContentLength != 600 {
		t.Fatalf("ContentLength = %d, want 600 after sizing", r.ContentLength)
	}

	err = hook(chunked(bytes.Repeat([]byte("b"), 600)))
	if d, ok := err.(*GateDenial); !ok || d.Rule != EventBlockedBudget {
		t.Fatalf("second 600-byte chunked call = %v, want a budget denial", err)
	}

	big := httptest.NewRequest(http.MethodPost, "http://ignored/v1/messages", io.NopCloser(io.LimitReader(zeroReader{}, anthproxyMaxBodyBytes+10)))
	big.ContentLength = -1
	if err := hook(big); err == nil {
		t.Fatal("an oversized chunked body was admitted, want a fail-closed refusal")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
