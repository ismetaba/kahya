package anthproxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// TestBudgetBlockedAlarmFiresOncePerProxy (FIX-I-2): every inbound request
// past the ceiling is still refused with 503 (CheckBeforeForward blocks each
// one), but the task-pause callback, the task_paused_budget ledger row and
// the alarm fire once per Proxy - the worker CLI retrying the 503 must not
// produce an alarm storm.
func TestBudgetBlockedAlarmFiresOncePerProxy(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	governor := NewGovernor(Limits{
		TaskTokenCeiling: 1, DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	ledger := &fakeLedger{}
	notifier := &fakeNotifier{}
	var paused atomic.Int32
	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, notifier)
	cfg.PauseBudget = func(context.Context, string) error { paused.Add(1); return nil }
	baseURL, _ := startTestProxy(t, cfg)

	for i := 0; i < 3; i++ {
		status, _ := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-sonnet-5-5","messages":[]}`))
		if status != http.StatusServiceUnavailable {
			t.Fatalf("request #%d status = %d, want 503 (budget block)", i, status)
		}
	}
	if got := notifier.countAlarm(EventTaskPausedBudget); got != 1 {
		t.Errorf("%s alarms = %d, want 1", EventTaskPausedBudget, got)
	}
	if got := paused.Load(); got != 1 {
		t.Errorf("PauseBudget calls = %d, want 1", got)
	}
	if got := ledger.countKind(EventTaskPausedBudget); got != 1 {
		t.Errorf("%s ledger rows = %d, want 1", EventTaskPausedBudget, got)
	}
	if hits := upstream.hits(); hits != 0 {
		t.Errorf("upstream hits = %d, want 0", hits)
	}
}
