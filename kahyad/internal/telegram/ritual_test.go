package telegram

import (
	"context"
	"testing"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
)

// fakeRitualAnswerer is a directly-controllable RitualAnswerer double.
type fakeRitualAnswerer struct {
	calls   []fakeRitualCall
	traceID string
	expired bool
	err     error
}

type fakeRitualCall struct {
	EvalLabelID int64
	Label       string
}

func (f *fakeRitualAnswerer) Answer(_ context.Context, evalLabelID int64, label string) (string, bool, error) {
	f.calls = append(f.calls, fakeRitualCall{EvalLabelID: evalLabelID, Label: label})
	if f.err != nil {
		return f.traceID, false, f.err
	}
	return f.traceID, f.expired, nil
}

// TestSendRitualQuestionByteExactTextAndButtons proves the question
// message text and every button label are byte-exact per the W5-03 task
// spec, and that the Hatirladi row rides underneath the answer row on the
// SAME message (never a second send).
func TestSendRitualQuestionByteExactTextAndButtons(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)

	ok := b.SendRitualQuestion(context.Background(), "trace-ritual-1", 42, "Emre pazartesileri yüzme kursuna gider")
	if !ok {
		t.Fatal("SendRitualQuestion = false, want true")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(sender.sent))
	}
	msg := sender.sent[0]
	wantText := "Bu doğru mu?\n\nEmre pazartesileri yüzme kursuna gider"
	if msg.Text != wantText {
		t.Errorf("text = %q, want %q", msg.Text, wantText)
	}
	if msg.Markup == nil || len(msg.Markup.InlineKeyboard) != 2 {
		t.Fatalf("markup = %+v, want exactly 2 rows (answers + hatirladi)", msg.Markup)
	}
	answerRow := msg.Markup.InlineKeyboard[0]
	if len(answerRow) != 3 {
		t.Fatalf("answer row = %+v, want 3 buttons", answerRow)
	}
	wantLabels := []string{btnDogru, btnYanlis, btnEminDegilim}
	for i, want := range wantLabels {
		if answerRow[i].Text != want {
			t.Errorf("answer row[%d].Text = %q, want %q", i, answerRow[i].Text, want)
		}
	}
	hatirladiRow := msg.Markup.InlineKeyboard[1]
	if len(hatirladiRow) != 1 || hatirladiRow[0].Text != btnRemembered {
		t.Fatalf("hatirladi row = %+v, want a single %q button", hatirladiRow, btnRemembered)
	}
}

// TestRitualCallbackRoutesActionToLabel proves each of the three button
// taps decodes to the correct ritual.Label* string and reaches
// RitualAnswerer.Answer with the right evalLabelID.
func TestRitualCallbackRoutesActionToLabel(t *testing.T) {
	cases := []struct {
		action byte
		want   string
	}{
		{cbActionRitualTrue, "true"},
		{cbActionRitualFalse, "false"},
		{cbActionRitualUnsure, "unsure"},
	}
	for _, tc := range cases {
		sender := &fakeSender{}
		answerer := &fakeRitualAnswerer{traceID: "trace-ritual-2"}
		b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
		b.SetRitualAnswerer(answerer)

		data := encodeRitualCallback(tc.action, 7)
		b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

		if len(answerer.calls) != 1 {
			t.Fatalf("action %q: Answer calls = %d, want 1", string(tc.action), len(answerer.calls))
		}
		if answerer.calls[0].EvalLabelID != 7 {
			t.Errorf("action %q: EvalLabelID = %d, want 7", string(tc.action), answerer.calls[0].EvalLabelID)
		}
		if answerer.calls[0].Label != tc.want {
			t.Errorf("action %q: Label = %q, want %q", string(tc.action), answerer.calls[0].Label, tc.want)
		}
		if len(sender.responded) != 1 || sender.responded[0] != toastRitualRecorded {
			t.Errorf("action %q: responded = %v, want [%q]", string(tc.action), sender.responded, toastRitualRecorded)
		}
	}
}

// TestRitualCallbackExpiredAnswersExpiredToast: an Answer reporting
// expired=true answers the expired toast, never the recorded one.
func TestRitualCallbackExpiredAnswersExpiredToast(t *testing.T) {
	sender := &fakeSender{}
	answerer := &fakeRitualAnswerer{traceID: "trace-ritual-3", expired: true}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.SetRitualAnswerer(answerer)

	data := encodeRitualCallback(cbActionRitualTrue, 1)
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

	if len(sender.responded) != 1 || sender.responded[0] != toastRitualExpired {
		t.Errorf("responded = %v, want [%q]", sender.responded, toastRitualExpired)
	}
}

// TestRitualCallbackUnavailableWhenUnwired: no RitualAnswerer wired at
// all degrades gracefully.
func TestRitualCallbackUnavailableWhenUnwired(t *testing.T) {
	sender := &fakeSender{}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)

	data := encodeRitualCallback(cbActionRitualTrue, 1)
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))

	if len(sender.responded) != 1 || sender.responded[0] != toastRitualFailed {
		t.Errorf("responded = %v, want [%q]", sender.responded, toastRitualFailed)
	}
}

// TestRitualCallbackDroppedOutsideAllowlist proves an out-of-allowlist tap
// on a ritual answer button is dropped silently and ledgered - the SAME
// W3-07 allowlistMiddleware, never a second implementation.
func TestRitualCallbackDroppedOutsideAllowlist(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	answerer := &fakeRitualAnswerer{traceID: "trace-ritual-4"}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
	b.SetRitualAnswerer(answerer)

	data := encodeRitualCallback(cbActionRitualTrue, 1)
	b.tb.ProcessUpdate(callbackUpdate(9999, 8888, data))

	if len(sender.responded) != 0 {
		t.Errorf("responded count = %d, want 0 (dropped silently)", len(sender.responded))
	}
	if len(answerer.calls) != 0 {
		t.Errorf("Answer calls = %d, want 0 (allowlist must gate BEFORE the answerer is ever reached)", len(answerer.calls))
	}
	if n := ledger.countKind("telegram_unauthorized_update"); n != 1 {
		t.Fatalf("telegram_unauthorized_update ledger rows = %d, want 1", n)
	}
}

// TestSendRitualQuestionEgressBlockedSendsNothing: a ritual run with the
// gate stubbed to DENY sends nothing (never reaches the fake sender) - the
// egress.Gate itself ledgers the denial (kahyad/internal/egress.Gate.Check's
// own record step), proven here by asserting the ledger carries a
// blocked-egress event and the sender saw zero sends.
func TestSendRitualQuestionEgressBlockedSendsNothing(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	// A real deny-everything Gate (empty allowlist), wired to THIS test's
	// own ledger (newDenyGate's shared helper always passes nil there) so
	// the gate's own denial ledgering is observable here.
	gate := egress.NewGate(policy.EgressConfig{}, nil, nil, ledger, nil, nil)
	b := newTestBot(t, testConfig(), sender, ledger, gate, nil, nil)

	ok := b.SendRitualQuestion(context.Background(), "trace-ritual-5", 1, "fact text")
	if ok {
		t.Fatal("SendRitualQuestion = true, want false (egress denied)")
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent count = %d, want 0", len(sender.sent))
	}
	if ledger.countKind("egress_blocked_allowlist") == 0 {
		t.Fatalf("expected the egress gate to ledger a denial event; got kinds: %v", ledgeredKinds(ledger))
	}
}

func ledgeredKinds(l *fakeLedger) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	for i, e := range l.events {
		out[i] = e.Kind
	}
	return out
}
