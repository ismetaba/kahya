// sendnotification_test.go covers W5-01's SendNotification: the morning
// briefing's own proactive, non-approval, non-alarm send path.
package telegram

import (
	"context"
	"testing"
)

// TestSendNotificationEgressAllowedReachesSender proves SendNotification
// passes through the SAME egress-gated send path as every other Telegram
// send this bot makes, and returns true on a genuine success.
func TestSendNotificationEgressAllowedReachesSender(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)

	ok := b.SendNotification(context.Background(), "trace-1", "Günaydın — sabah brifingi\ntrace_id: trace-1\n")
	if !ok {
		t.Fatal("SendNotification = false, want true (egress allowed, sender succeeds)")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(sender.sent))
	}
	if sender.sent[0].Text != "Günaydın — sabah brifingi\ntrace_id: trace-1\n" {
		t.Errorf("sent text = %q, want the exact text passed in (no mutation)", sender.sent[0].Text)
	}
}

// TestSendNotificationEgressBlockedReturnsFalse proves a blocked egress
// decision degrades SendNotification to false (never a panic, never a
// silently-assumed success) - the caller (kahyad/internal/briefing) relies
// on this to decide whether to ledger briefing.delivered.
func TestSendNotificationEgressBlockedReturnsFalse(t *testing.T) {
	sender := &fakeSender{}
	local := &fakeLocalNotifier{}
	b := newTestBot(t, testConfig(), sender, nil, newDenyGate(t), nil, local)

	ok := b.SendNotification(context.Background(), "trace-2", "some briefing text")
	if ok {
		t.Fatal("SendNotification = true, want false (egress denied)")
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent count = %d, want 0 (never reaches the sender when egress denies)", len(sender.sent))
	}
}

// TestSendNotificationDisabledBotReturnsFalse proves a disabled bot
// (New() with no chat_id/user_id configured) degrades SendNotification to
// false with no attempt to touch the egress gate or sender at all.
func TestSendNotificationDisabledBotReturnsFalse(t *testing.T) {
	b := New(Config{}, &neverCalledTokenReader{t: t}, nil, nil, nil, nil, "", nil, nil)
	if ok := b.SendNotification(context.Background(), "trace-3", "text"); ok {
		t.Fatal("SendNotification on a disabled bot = true, want false")
	}
}
