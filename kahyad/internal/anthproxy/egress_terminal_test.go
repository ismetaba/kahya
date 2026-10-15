package anthproxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/secretlane"
)

// FIX-U-4: kahyad's OWN egress-gate 403 for a secret-lane task
// (secretlane.ErrCloudBlocked - sticky tasks.lane) is a terminal verdict:
// the task fails through OnNonRetryableFailure exactly once per Proxy,
// whatever the worker's own terminal line later says.

func egressRequest(t *testing.T, baseURL string) (int, string) {
	t.Helper()
	return doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-sonnet-5-5"}`))
}

func TestProxyEgress403FailsTaskExactlyOnce(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{200, 200, 200}) // must stay at 0 hits
	ledger := &fakeLedger{}
	cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
	rec := &nonRetryableRecorder{}
	cfg.OnNonRetryableFailure = rec.hook
	cfg.EgressGate = secretlane.NewProxyBackstopHook(&fakeLaneLookup{lane: secretlane.LaneSecret}, ledger)("t_secret", "trace_secret")
	baseURL, _ := startTestProxy(t, cfg)

	for i := 0; i < 3; i++ { // the CLI may re-issue the call; kahyad fails the task ONCE
		status, body := egressRequest(t, baseURL)
		if status != http.StatusForbidden || !strings.Contains(body, secretlane.MsgSecretLaneCloudBlocked) {
			t.Fatalf("#%d status=%d body=%s, want 403 with the secret-lane message", i, status, body)
		}
	}
	reasons := rec.snapshot()
	if len(reasons) != 1 {
		t.Fatalf("OnNonRetryableFailure fired %d times (%v), want exactly 1", len(reasons), reasons)
	}
	if reasons[0] != ReasonSecretLaneCloudBlocked {
		t.Fatalf("reason = %q, want %q", reasons[0], ReasonSecretLaneCloudBlocked)
	}
	if rec.taskIDs[0] != cfg.TaskID {
		t.Fatalf("callback task = %q, want %q", rec.taskIDs[0], cfg.TaskID)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0", got)
	}
	if got := ledger.countKind(secretlane.EventSecretLaneCloudBlocked); got != 3 {
		t.Fatalf("secretlane_cloud_blocked = %d, want 3 (one per 403 - unchanged by this fix)", got)
	}
	if got := ledger.countKind(EventEgressBlockedTerminal); got != 1 {
		t.Fatalf("%s = %d, want 1", EventEgressBlockedTerminal, got)
	}
}

// fakeGateDenial mimics egress.GateDenial (anthproxy never imports
// egress; TestNewAnthproxyEgressGateHook_DenialCarriesTerminalReason pins
// the real type's TerminalEgressReason values).
type fakeGateDenial struct{ reason, terminal string }

func (d *fakeGateDenial) Error() string                { return d.reason }
func (d *fakeGateDenial) TerminalEgressReason() string { return d.terminal }

// TestProxyEgressAllowlistDenialUsesEgressBlockedReason (FIX-U-4 acceptance,
// FIX-FINAL u4-allowlist-sensitive-403-not-terminal): a deterministic W3-05
// allowlist / sensitive-read denial is terminal too - every request still
// gets its 403 carrying the Turkish reason, but the task fails exactly
// once with ReasonEgressBlocked, and nothing reaches the upstream.
func TestProxyEgressAllowlistDenialUsesEgressBlockedReason(t *testing.T) {
	for _, name := range []string{"allowlist", "sensitive"} {
		t.Run(name, func(t *testing.T) {
			upstream := newStatusSequenceUpstream(t, []int{200, 200})
			ledger := &fakeLedger{}
			cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
			rec := &nonRetryableRecorder{}
			cfg.OnNonRetryableFailure = rec.hook
			denial := &fakeGateDenial{reason: "Egress engellendi (" + name + ")", terminal: ReasonEgressBlocked}
			cfg.EgressGate = func(*http.Request) error { return denial }
			baseURL, _ := startTestProxy(t, cfg)
			for i := 0; i < 2; i++ {
				if status, body := egressRequest(t, baseURL); status != http.StatusForbidden || !strings.Contains(body, denial.reason) {
					t.Fatalf("#%d status=%d body=%s, want 403 carrying the gate reason", i, status, body)
				}
			}
			reasons := rec.snapshot()
			if len(reasons) != 1 || reasons[0] != ReasonEgressBlocked {
				t.Fatalf("OnNonRetryableFailure reasons = %v, want exactly [%q]", reasons, ReasonEgressBlocked)
			}
			if got := ledger.countKind(EventEgressBlockedTerminal); got != 1 {
				t.Fatalf("%s = %d, want 1", EventEgressBlockedTerminal, got)
			}
			if got := upstream.hits(); got != 0 {
				t.Fatalf("upstream hits = %d, want 0", got)
			}
		})
	}
}

// TestProxyEgressNonSecretDenialIsNotTerminal: only the secret-lane verdict
// and a deterministic allowlist / sensitive-read denial are terminal. A
// lane-lookup failure (ErrCloudBlockedLookupFailed - transient: the next
// call may read the lane fine), a volume-budget denial (daily) and a plain
// gate error still get their 403 but never fail the task from here (the
// worker's explicit error line does, FIX-I-2).
func TestProxyEgressNonSecretDenialIsNotTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"lane lookup failed", secretlane.ErrCloudBlockedLookupFailed},
		{"budget denial", &fakeGateDenial{reason: "Egress bütçesi aşıldı", terminal: ""}},
		{"plain gate error", errors.New("egress gate: budget lookup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newStatusSequenceUpstream(t, []int{200})
			ledger := &fakeLedger{}
			cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
			rec := &nonRetryableRecorder{}
			cfg.OnNonRetryableFailure = rec.hook
			cfg.EgressGate = func(*http.Request) error { return tc.err }
			baseURL, _ := startTestProxy(t, cfg)
			for i := 0; i < 2; i++ {
				if status, body := egressRequest(t, baseURL); status != http.StatusForbidden || !strings.Contains(body, tc.err.Error()) {
					t.Fatalf("#%d status=%d body=%s, want 403 carrying the gate error", i, status, body)
				}
			}
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("OnNonRetryableFailure fired %v for a non-secret-lane denial, want never", got)
			}
			if got := ledger.countKind(EventEgressBlockedTerminal); got != 0 {
				t.Fatalf("%s = %d, want 0", EventEgressBlockedTerminal, got)
			}
			if got := upstream.hits(); got != 0 {
				t.Fatalf("upstream hits = %d, want 0", got)
			}
		})
	}
}

// TestProxyEgressGateAllowDoesNotFireCallback: a gate that allows changes
// nothing (regression guard).
func TestProxyEgressGateAllowDoesNotFireCallback(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{200})
	ledger := &fakeLedger{}
	cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
	rec := &nonRetryableRecorder{}
	cfg.OnNonRetryableFailure = rec.hook
	cfg.EgressGate = func(*http.Request) error { return nil }
	baseURL, _ := startTestProxy(t, cfg)
	if status, body := egressRequest(t, baseURL); status != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", status, body)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("callback fired %v on an allowed call", got)
	}
	if got := upstream.hits(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	if got := ledger.countKind(EventEgressBlockedTerminal); got != 0 {
		t.Fatalf("%s = %d, want 0", EventEgressBlockedTerminal, got)
	}
}

// TestProxyEgress403WithNilCallbackStillLedgers: no callback wired (the
// reader's / a test's proxy) - no panic, the terminal verdict is still
// ledgered once.
func TestProxyEgress403WithNilCallbackStillLedgers(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{200})
	ledger := &fakeLedger{}
	cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
	cfg.OnNonRetryableFailure = nil
	cfg.EgressGate = func(*http.Request) error { return secretlane.ErrCloudBlocked }
	baseURL, _ := startTestProxy(t, cfg)
	for i := 0; i < 2; i++ {
		if status, _ := egressRequest(t, baseURL); status != http.StatusForbidden {
			t.Fatalf("#%d status=%d, want 403", i, status)
		}
	}
	if got := ledger.countKind(EventEgressBlockedTerminal); got != 1 {
		t.Fatalf("%s = %d, want 1", EventEgressBlockedTerminal, got)
	}
}

// TestProxyEgressRealAllowlistHookIsTerminal wires the REAL production
// hook (egress.NewAnthproxyEgressGateHook) with an allowlist that does not
// cover the upstream: the deny it returns must be recognised as terminal
// here (ReasonEgressBlocked == egress.TerminalEgressReason).
func TestProxyEgressRealAllowlistHookIsTerminal(t *testing.T) {
	if egress.TerminalEgressReason != ReasonEgressBlocked {
		t.Fatalf("egress.TerminalEgressReason %q != ReasonEgressBlocked %q", egress.TerminalEgressReason, ReasonEgressBlocked)
	}
	upstream := newStatusSequenceUpstream(t, []int{200})
	ledger := &fakeLedger{}
	cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
	rec := &nonRetryableRecorder{}
	cfg.OnNonRetryableFailure = rec.hook
	gate := egress.NewGate(policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "other.example"}},
		DefaultDailyByteBudget: 1 << 20,
	}, egress.NewSensitiveTracker(), nil, nil, nil, nil)
	cfg.EgressGate = egress.NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)(cfg.TaskID, cfg.TraceID)
	baseURL, _ := startTestProxy(t, cfg)
	for i := 0; i < 2; i++ {
		if status, _ := egressRequest(t, baseURL); status != http.StatusForbidden {
			t.Fatalf("#%d status=%d, want 403", i, status)
		}
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != ReasonEgressBlocked {
		t.Fatalf("OnNonRetryableFailure reasons = %v, want exactly [%q]", got, ReasonEgressBlocked)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0", got)
	}
}
