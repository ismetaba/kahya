package notify

import (
	"context"
	"testing"

	"kahya/kahyad/internal/logx"
)

type fakeLedger struct {
	traceID string
	kind    string
	payload map[string]any
	calls   int
}

func (f *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.traceID = traceID
	f.kind = kind
	f.payload = payload
	f.calls++
	return nil
}

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New() error = %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func TestNotifyLedgersAndCarriesMessage(t *testing.T) {
	ledger := &fakeLedger{}
	n := New(testLogger(t), ledger)

	err := n.Notify(context.Background(), "trace1", "keychain_unavailable",
		"Keychain erişilemiyor — bulut şeridi kapalı.", map[string]any{"task_id": "t_abc"})
	if err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if ledger.calls != 1 {
		t.Fatalf("ledger.calls = %d, want 1", ledger.calls)
	}
	if ledger.kind != "keychain_unavailable" {
		t.Errorf("ledger.kind = %q, want keychain_unavailable", ledger.kind)
	}
	if ledger.traceID != "trace1" {
		t.Errorf("ledger.traceID = %q, want trace1", ledger.traceID)
	}
	if ledger.payload["message"] != "Keychain erişilemiyor — bulut şeridi kapalı." {
		t.Errorf("ledger.payload[message] = %v, want the Turkish message verbatim", ledger.payload["message"])
	}
	if ledger.payload["task_id"] != "t_abc" {
		t.Errorf("ledger.payload[task_id] = %v, want t_abc", ledger.payload["task_id"])
	}
}

func TestAlarmLedgers(t *testing.T) {
	ledger := &fakeLedger{}
	n := New(testLogger(t), ledger)

	if err := n.Alarm(context.Background(), "trace2", "spend_alarm_80", "Gunluk harcama %80'e ulasti.", nil); err != nil {
		t.Fatalf("Alarm() error = %v", err)
	}
	if ledger.calls != 1 || ledger.kind != "spend_alarm_80" {
		t.Fatalf("ledger not called as expected: calls=%d kind=%q", ledger.calls, ledger.kind)
	}
}

func TestNilDependenciesAreNoOps(t *testing.T) {
	n := New(nil, nil)
	if err := n.Notify(context.Background(), "t", "k", "m", nil); err != nil {
		t.Fatalf("Notify() with nil log/ledger error = %v, want nil", err)
	}
	if err := n.Alarm(context.Background(), "t", "k", "m", nil); err != nil {
		t.Fatalf("Alarm() with nil log/ledger error = %v, want nil", err)
	}
}
