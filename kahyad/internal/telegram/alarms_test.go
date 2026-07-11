package telegram

import (
	"context"
	"strings"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/notify"
)

// stubInnerNotifier is a minimal notify.Notifier fake so alarms_test.go
// can assert AlarmNotifier's OWN delegation/fan-out behavior in isolation
// from the real JSONLNotifier's log/ledger I/O.
type stubInnerNotifier struct {
	notifyCalls []string
	alarmCalls  []string
}

func (s *stubInnerNotifier) Notify(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	s.notifyCalls = append(s.notifyCalls, kind)
	return nil
}
func (s *stubInnerNotifier) Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error {
	s.alarmCalls = append(s.alarmCalls, kind)
	return nil
}

var _ notify.Notifier = (*stubInnerNotifier)(nil)

// TestAlarmNotifierNotifyDelegatesOnly proves plain Notify() calls never
// reach Telegram - only Alarm-class events escalate.
func TestAlarmNotifierNotifyDelegatesOnly(t *testing.T) {
	sender := &fakeSender{}
	inner := &stubInnerNotifier{}
	bot := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	an := NewAlarmNotifier(inner, bot)

	if err := an.Notify(context.Background(), "trace-1", "keychain_unavailable", "Keychain erişilemiyor", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(inner.notifyCalls) != 1 {
		t.Fatalf("inner.Notify calls = %d, want 1", len(inner.notifyCalls))
	}
	if len(sender.sent) != 0 {
		t.Fatalf("Notify() must never reach Telegram, got %d sent message(s)", len(sender.sent))
	}
}

// TestAlarmNotifierFansOutToTelegram is the task spec's own acceptance
// criterion: "Cost alarm path: fire W12-08's ceiling hook in a test ⇒
// Turkish alarm message queued through the egress-checked sender." This
// drives the REAL kahyad/internal/anthproxy.Governor.CheckBeforeForward
// ceiling check (W12-08's own logic, unmodified) and mirrors proxy.go's
// own onBudgetBlocked call shape exactly, so the alarm this test fires is
// the SAME one production would.
func TestAlarmNotifierFansOutToTelegram(t *testing.T) {
	sender := &fakeSender{}
	inner := &stubInnerNotifier{}
	bot := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	an := NewAlarmNotifier(inner, bot)

	gov := anthproxy.NewGovernor(anthproxy.Limits{TaskTokenCeiling: 10}, nil, an)
	check := gov.CheckBeforeForward("task-ceiling", "claude-sonnet-5", nil)
	if check.Allowed {
		t.Fatalf("CheckBeforeForward with a 10-token ceiling and a 50000-token fallback estimate must be blocked")
	}
	if check.Message != anthproxy.MsgTaskCeiling {
		t.Fatalf("check.Message = %q, want anthproxy.MsgTaskCeiling", check.Message)
	}

	// This is EXACTLY what kahyad/internal/anthproxy's proxy.go's
	// onBudgetBlocked does with the governor's own CheckResult.
	if err := an.Alarm(context.Background(), "trace-ceiling", anthproxy.EventTaskPausedBudget, check.Message, map[string]any{"task_id": "task-ceiling"}); err != nil {
		t.Fatalf("Alarm: %v", err)
	}

	if len(inner.alarmCalls) != 1 {
		t.Fatalf("inner.Alarm calls = %d, want 1 (the JSONL+ledger write must still happen)", len(inner.alarmCalls))
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 Telegram alarm message", len(sender.sent))
	}
	text := sender.sent[0].Text
	if !strings.Contains(text, "⚠️ Görev duraklatıldı:") {
		t.Errorf("alarm text = %q, want the fixed Turkish prefix", text)
	}
	if !strings.Contains(text, anthproxy.MsgTaskCeiling) {
		t.Errorf("alarm text = %q, want it to include the governor's own message", text)
	}
	if !strings.Contains(text, "trace-ceiling") {
		t.Errorf("alarm text = %q, want it to include the trace id", text)
	}
	if sender.sent[0].Markup != nil {
		t.Error("an alarm message must never carry an approval keyboard")
	}
}

// TestAlarmNotifierBlockedEgressFallsBackLocally proves an egress-blocked
// alarm send falls back to the local notifier rather than being silently
// dropped (task spec step 5).
func TestAlarmNotifierBlockedEgressFallsBackLocally(t *testing.T) {
	sender := &fakeSender{}
	inner := &stubInnerNotifier{}
	local := &fakeLocalNotifier{}
	bot := newTestBot(t, testConfig(), sender, nil, newDenyGate(t), nil, local)
	an := NewAlarmNotifier(inner, bot)

	if err := an.Alarm(context.Background(), "trace-blocked", anthproxy.EventSpendAlarm80, "Gunluk harcama %80 esigini asti.", nil); err != nil {
		t.Fatalf("Alarm: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent count = %d, want 0 (egress denied)", len(sender.sent))
	}
	if len(local.calls) != 1 {
		t.Fatalf("local fallback calls = %d, want 1", len(local.calls))
	}
}

// TestFormatAlarmTextCacheHitPercent covers the task spec's own literal
// example: "📉 Cache-hit oranı düştü: %<n>".
func TestFormatAlarmTextCacheHitPercent(t *testing.T) {
	got := formatAlarmText(kindCacheHitAlarm, "Gunluk cache-hit orani esigin altina dustu.", "trace-x", map[string]any{"pct": 37})
	want := "📉 Cache-hit oranı düştü: %37 (trace: trace-x)"
	if got != want {
		t.Errorf("formatAlarmText = %q, want %q", got, want)
	}
}

// TestFormatAlarmTextFallsBackWithoutPayload proves the current
// production shape (governor.go passes a nil payload for the cache-hit
// alarm today) still yields a fully informative line.
func TestFormatAlarmTextFallsBackWithoutPayload(t *testing.T) {
	got := formatAlarmText(kindCacheHitAlarm, "Gunluk cache-hit orani esigin altina dustu.", "trace-y", nil)
	want := "📉 Cache-hit oranı düştü: Gunluk cache-hit orani esigin altina dustu. (trace: trace-y)"
	if got != want {
		t.Errorf("formatAlarmText = %q, want %q", got, want)
	}
}
