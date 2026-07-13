package telegram

import (
	"context"
	"testing"
)

// fakeRememberedMarker is a directly-controllable RememberedMarker double.
type fakeRememberedMarker struct {
	calls     []fakeRememberedCall
	duplicate bool
	err       error
}

type fakeRememberedCall struct {
	TraceID string
	Channel string
}

func (f *fakeRememberedMarker) Mark(_ context.Context, traceID, channel string) (bool, error) {
	f.calls = append(f.calls, fakeRememberedCall{TraceID: traceID, Channel: channel})
	if f.err != nil {
		return false, f.err
	}
	return f.duplicate, nil
}

// TestHatirladiButtonMarksRemoteChannel proves a tap routes into
// RememberedMarker.Mark with channel="remote" (Telegram marks are always
// remote, HANDOFF §5 safety #5) and answers the byte-exact success toast.
func TestHatirladiButtonMarksRemoteChannel(t *testing.T) {
	sender := &fakeSender{}
	marker := &fakeRememberedMarker{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.SetRememberedMarker(marker)

	traceID := "abcd0000000000000000000000000001"
	data := encodeRememberedCallback(traceID)
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

	if len(marker.calls) != 1 {
		t.Fatalf("Mark calls = %d, want 1", len(marker.calls))
	}
	if marker.calls[0].TraceID != traceID {
		t.Errorf("Mark traceID = %q, want %q", marker.calls[0].TraceID, traceID)
	}
	if marker.calls[0].Channel != "remote" {
		t.Errorf("Mark channel = %q, want %q", marker.calls[0].Channel, "remote")
	}
	if len(sender.responded) != 1 || sender.responded[0] != toastRememberedSaved {
		t.Errorf("responded = %v, want [%q]", sender.responded, toastRememberedSaved)
	}
}

// TestHatirladiButtonDuplicateTapAnswersDuplicateToast: a double-tap
// (Mark reports duplicate=true) answers the duplicate toast, never the
// saved one - the mark itself is still exactly once (Marker's own job,
// verified in kahyad/internal/remembered's tests; this test only proves
// the bot surfaces the duplicate result correctly).
func TestHatirladiButtonDuplicateTapAnswersDuplicateToast(t *testing.T) {
	sender := &fakeSender{}
	marker := &fakeRememberedMarker{duplicate: true}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.SetRememberedMarker(marker)

	data := encodeRememberedCallback("abcd0000000000000000000000000002")
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

	if len(sender.responded) != 1 || sender.responded[0] != toastRememberedDuplicate {
		t.Errorf("responded = %v, want [%q]", sender.responded, toastRememberedDuplicate)
	}
}

// TestHatirladiButtonUnavailableWhenUnwired: no RememberedMarker wired at
// all degrades gracefully (a toast, never a panic).
func TestHatirladiButtonUnavailableWhenUnwired(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)

	data := encodeRememberedCallback("abcd0000000000000000000000000003")
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

	if len(sender.responded) != 1 || sender.responded[0] != toastRememberedUnavailable {
		t.Errorf("responded = %v, want [%q]", sender.responded, toastRememberedUnavailable)
	}
}

// TestHatirladiButtonDroppedOutsideAllowlist proves an out-of-allowlist
// tap is dropped silently (no toast) and ledgered - reusing the SAME W3-07
// allowlistMiddleware every other callback in this package goes through;
// RememberedMarker.Mark must never even be called.
func TestHatirladiButtonDroppedOutsideAllowlist(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	marker := &fakeRememberedMarker{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
	b.SetRememberedMarker(marker)

	data := encodeRememberedCallback("abcd0000000000000000000000000004")
	b.tb.ProcessUpdate(callbackUpdate(9999, 8888, data))

	if len(sender.responded) != 0 {
		t.Errorf("responded count = %d, want 0 (dropped silently)", len(sender.responded))
	}
	if len(marker.calls) != 0 {
		t.Errorf("Mark calls = %d, want 0 (allowlist must gate BEFORE the marker is ever reached)", len(marker.calls))
	}
	if n := ledger.countKind("telegram_unauthorized_update"); n != 1 {
		t.Fatalf("telegram_unauthorized_update ledger rows = %d, want 1", n)
	}
}

// TestSendTaskResultAttachesHatirladiButton proves SendTaskResult's
// markup carries exactly one "🌟 Hatırladı" button, keyed on traceID, and
// that the send itself passes through the SAME egress-gated path.
func TestSendTaskResultAttachesHatirladiButton(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)

	ok := b.SendTaskResult(context.Background(), "trace-result-1", "Görev tamamlandı: rapor hazır.")
	if !ok {
		t.Fatal("SendTaskResult = false, want true")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(sender.sent))
	}
	markup := sender.sent[0].Markup
	if markup == nil || len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("markup = %+v, want exactly one row with one button", markup)
	}
	btn := markup.InlineKeyboard[0][0]
	if btn.Text != btnRemembered {
		t.Errorf("button text = %q, want %q", btn.Text, btnRemembered)
	}
	if btn.Data != encodeRememberedCallback("trace-result-1") {
		t.Errorf("button data = %q, want the trace-id-keyed callback data", btn.Data)
	}
}

// TestSendTaskResultEgressBlockedSendsNothing: gate-DENY sends nothing
// (HANDOFF §5 safety #1 - task-result deliveries are egress too, same
// gate as everything else).
func TestSendTaskResultEgressBlockedSendsNothing(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newDenyGate(t), nil, nil)

	if ok := b.SendTaskResult(context.Background(), "trace-result-2", "text"); ok {
		t.Fatal("SendTaskResult = true, want false (egress denied)")
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent count = %d, want 0", len(sender.sent))
	}
}
