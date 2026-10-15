package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
)

// FIX-U-5: every content-carrying Telegram body - not only approval cards -
// is gated by the session (sensitive-read mark) and by the deterministic
// secret-lane pre-pass before it leaves (HANDOFF §5 safety #1 ⚑ second
// clause, safety #5 ⚑).

// fakeSessionTaint mirrors FIX-B-7's IsMarkedCtx contract: (true, err) on a
// store error, so a test can prove the caller does not trust the bool alone.
type fakeSessionTaint struct {
	marked bool
	err    error
}

func (f *fakeSessionTaint) IsMarkedCtx(context.Context, string) (bool, error) {
	return f.marked || f.err != nil, f.err
}

// newMarkedBot builds an online test bot whose SessionTaint already carries
// traceID (egress.NewSensitiveTracker + Mark - the in-memory half every
// production mark also sets), with an allow gate, a ledger and a local
// notifier.
func newMarkedBot(t *testing.T, traceID string, engine FeedbackEngine) (*Bot, *fakeSender, *fakeLedger, *fakeLocalNotifier) {
	t.Helper()
	sender, ledger, local := &fakeSender{}, &fakeLedger{}, &fakeLocalNotifier{}
	sessions := egress.NewSensitiveTracker()
	sessions.Mark(traceID)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), engine, local)
	b.SetSessionTaint(sessions)
	return b, sender, ledger, local
}

// textRedactedRows returns every telegram_text_redacted row's payload.
func textRedactedRows(l *fakeLedger) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, e := range l.events {
		if e.Kind == "telegram_text_redacted" {
			out = append(out, e.Payload)
		}
	}
	return out
}

// assertGenericNotice: exactly want sends, each the byte-exact
// msgOutboundRedactedFmt for traceID with no keyboard.
func assertGenericNotice(t *testing.T, sender *fakeSender, traceID string, want int) {
	t.Helper()
	if len(sender.sent) != want {
		t.Fatalf("sent = %q, want %d redacted notice(s)", sender.allTexts(), want)
	}
	notice := fmt.Sprintf(msgOutboundRedactedFmt, traceID)
	for i, m := range sender.sent {
		if m.Text != notice {
			t.Fatalf("sent[%d] = %q, want byte-exact %q", i, m.Text, notice)
		}
		if m.Markup != nil {
			t.Fatalf("sent[%d] carries a keyboard: %+v", i, m.Markup)
		}
	}
}

// TestSendTaskResultRedactsSecretLaneText (content half): an unmarked
// session's task result carrying an IBAN leaves only as the constant
// notice - no IBAN byte, no Hatırladı button - and SendTaskResult reports
// false; the redaction is ledgered with its reason only, and the local
// notifier is told.
func TestSendTaskResultRedactsSecretLaneText(t *testing.T) {
	sender, ledger, local := &fakeSender{}, &fakeLedger{}, &fakeLocalNotifier{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, local)

	const trace = "trace-u5-1"
	if ok := b.SendTaskResult(context.Background(), trace, "Görev tamamlandı: ödeme IBAN "+testIBAN+" ile yapıldı."); ok {
		t.Fatal("SendTaskResult = true, want false (the result itself never reached Telegram)")
	}
	assertGenericNotice(t, sender, trace, 1)
	all := strings.Join(sender.allTexts(), "\n")
	for _, bad := range []string{"TR33", "6457", "ödeme", "tamamlandı"} {
		if strings.Contains(all, bad) {
			t.Fatalf("Telegram got %q: %q", bad, all)
		}
	}
	rows := textRedactedRows(ledger)
	if len(rows) != 1 {
		t.Fatalf("telegram_text_redacted rows = %d, want 1", len(rows))
	}
	if reason, _ := rows[0]["reason"].(string); !strings.HasPrefix(reason, "content:iban") {
		t.Fatalf("reason = %q, want content:iban...", reason)
	}
	if kb, _ := rows[0]["keyboard"].(bool); !kb {
		t.Fatalf("keyboard = %v, want true (SendTaskResult attaches the Hatırladı button)", rows[0]["keyboard"])
	}
	for k := range rows[0] {
		if k != "event" && k != "reason" && k != "bytes" && k != "keyboard" {
			t.Fatalf("telegram_text_redacted payload carries %q; only event/reason/bytes/keyboard allowed (never the text)", k)
		}
	}
	if want := []string{"telegram_text_redacted:" + msgTextRedactedLocal}; strings.Join(local.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("local notifier calls = %q, want %q", local.calls, want)
	}
}

// TestSendNotificationFromSensitiveSessionRedacted (session half): a
// session that did a secret-lane read sends even CLEAN text only as the
// constant notice; the mark is per trace, not per bot.
func TestSendNotificationFromSensitiveSessionRedacted(t *testing.T) {
	const trace = "trace-u5-2"
	b, sender, ledger, _ := newMarkedBot(t, trace, nil)

	if ok := b.SendNotification(context.Background(), trace, "Günaydın — sabah brifingi\ntrace_id: "+trace+"\n\n- bugün hava güzel\n"); ok {
		t.Fatal("SendNotification from a sensitive session = true, want false")
	}
	assertGenericNotice(t, sender, trace, 1)
	rows := textRedactedRows(ledger)
	if len(rows) != 1 || rows[0]["reason"] != "session:sensitive_read" {
		t.Fatalf("telegram_text_redacted rows = %+v, want one with reason session:sensitive_read", rows)
	}

	const clean = "- bugün hava güzel\n"
	if ok := b.SendNotification(context.Background(), "trace-u5-2-other", clean); !ok {
		t.Fatal("SendNotification from an unmarked trace = false, want true")
	}
	if len(sender.sent) != 2 || sender.sent[1].Text != clean {
		t.Fatalf("sent = %q, want the unmarked trace's text verbatim", sender.allTexts())
	}
	if strings.Contains(sender.sent[0].Text, "hava") {
		t.Fatalf("the marked session's text leaked: %q", sender.sent[0].Text)
	}
}

// TestAlarmFromSensitiveSessionRedacted: an alarm raised under a marked
// session still reaches inner (JSONL + ledger) in full, but Telegram gets
// only the constant notice (decision U5-1: no alarm exemption).
func TestAlarmFromSensitiveSessionRedacted(t *testing.T) {
	const trace = "trace-u5-3"
	b, sender, _, _ := newMarkedBot(t, trace, nil)
	inner := &stubInnerNotifier{}
	an := NewAlarmNotifier(inner, b)

	if err := an.Alarm(context.Background(), trace, anthproxy.EventTaskPausedBudget, anthproxy.MsgTaskCeiling, nil); err != nil {
		t.Fatalf("Alarm: %v", err)
	}
	if len(inner.alarmCalls) != 1 {
		t.Fatalf("inner.Alarm calls = %d, want 1 (the local record is never affected)", len(inner.alarmCalls))
	}
	assertGenericNotice(t, sender, trace, 1)
	if strings.Contains(sender.sent[0].Text, "duraklatıldı") {
		t.Fatalf("alarm text leaked: %q", sender.sent[0].Text)
	}
}

// TestPendingApprovalFromSensitiveSessionRedactedTitleOnly: a marked
// session's approval card is decided in OnPendingApproval before render -
// the tool-named redacted title, no keyboard, nothing tracked, and send's
// backstop never runs (no telegram_text_redacted, no incomplete card).
func TestPendingApprovalFromSensitiveSessionRedactedTitleOnly(t *testing.T) {
	const trace = "trace-u5-4"
	fix := newPolicyFixture(t)
	b, sender, ledger, _ := newMarkedBot(t, trace, fix.Engine)

	toolInput := fsWriteInput(t, "~/Belgeler/gunluk-not.txt", "bugün hava güzel")
	info := mintPending(t, fix, "fs_write", policy.ClassW2, toolInput, trace)
	b.OnPendingApproval(context.Background(), info)

	if len(sender.sent) != 1 {
		t.Fatalf("sent = %q, want exactly the redacted title", sender.allTexts())
	}
	if want := "🔒 Yerel onay gerekiyor: fs_write (gizli şerit)"; sender.sent[0].Text != want {
		t.Fatalf("sent = %q, want %q (tool-named title, not the generic notice)", sender.sent[0].Text, want)
	}
	if sender.sent[0].Markup != nil {
		t.Fatalf("redacted title carries a keyboard: %+v", sender.sent[0].Markup)
	}
	if n := cardCount(b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0", n)
	}
	if reasons := redactedReasons(ledger); len(reasons) != 1 || reasons[0] != "session:sensitive_read" {
		t.Fatalf("telegram_card_redacted reasons = %q, want [session:sensitive_read]", reasons)
	}
	if n := ledger.countKind("telegram_card_incomplete"); n != 0 {
		t.Fatalf("telegram_card_incomplete = %d, want 0", n)
	}
	if n := ledger.countKind("telegram_text_redacted"); n != 0 {
		t.Fatalf("telegram_text_redacted = %d, want 0 (the early check decided, not the backstop)", n)
	}
}

// TestSessionTaintLookupErrorFailsClosed: a sensitive-read lookup error
// restricts the text (CLAUDE.md fail-closed) and is ledgered with its error
// only.
func TestSessionTaintLookupErrorFailsClosed(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
	b.SetSessionTaint(&fakeSessionTaint{err: errors.New("disk")})

	const trace = "trace-u5-5"
	if ok := b.SendNotification(context.Background(), trace, "merhaba"); ok {
		t.Fatal("SendNotification with a failing taint lookup = true, want false")
	}
	assertGenericNotice(t, sender, trace, 1)
	ledger.mu.Lock()
	var lookupRows []map[string]any
	for _, e := range ledger.events {
		if e.Kind == "telegram_session_taint_lookup_failed" {
			lookupRows = append(lookupRows, e.Payload)
		}
	}
	ledger.mu.Unlock()
	if len(lookupRows) != 1 || lookupRows[0]["err"] != "disk" || len(lookupRows[0]) != 2 {
		t.Fatalf("telegram_session_taint_lookup_failed rows = %+v, want one {event, err: disk}", lookupRows)
	}
	if rows := textRedactedRows(ledger); len(rows) != 1 || rows[0]["reason"] != "session:lookup_failed" {
		t.Fatalf("telegram_text_redacted rows = %+v, want one with reason session:lookup_failed", rows)
	}
}

// TestSendWithoutSessionTaintFailsClosed (decision U5-2): an unwired
// SessionTaint does not disable the gate - every content send is redacted.
func TestSendWithoutSessionTaintFailsClosed(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
	b.taint = nil

	const trace = "trace-u5-unwired"
	for i := 0; i < 2; i++ {
		if ok := b.SendNotification(context.Background(), trace, "merhaba"); ok {
			t.Fatalf("SendNotification #%d with no SessionTaint wired = true, want false", i+1)
		}
	}
	assertGenericNotice(t, sender, trace, 2)
	rows := textRedactedRows(ledger)
	if len(rows) != 2 || rows[0]["reason"] != "session:unwired" || rows[1]["reason"] != "session:unwired" {
		t.Fatalf("telegram_text_redacted rows = %+v, want two with reason session:unwired", rows)
	}
}

// TestSendWithoutSessionTaintLogsUnwiredOnce: the unwired warning is one
// JSONL line per process, however many sends are redacted.
func TestSendWithoutSessionTaintLogsUnwiredOnce(t *testing.T) {
	log, lines := newJSONLTestLogger(t)
	b := newTestBot(t, testConfig(), &fakeSender{}, nil, newAllowGate(t), nil, nil)
	b.log = log
	b.taint = nil
	for i := 0; i < 3; i++ {
		b.SendNotification(context.Background(), "trace-u5-unwired-log", "merhaba")
	}
	if n := len(eventsNamed(lines(), "telegram_session_taint_unwired")); n != 1 {
		t.Fatalf("telegram_session_taint_unwired lines = %d, want 1", n)
	}
}

// TestRedactedNoticeItselfIsEgressChecked: the constant notice is still an
// off-box byte - it passes egress.Check under the same session key
// (HANDOFF §5 #1 "every off-box byte"); Sender.Send is never reached around
// the gate.
func TestRedactedNoticeItselfIsEgressChecked(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)
	sessions := egress.NewSensitiveTracker()
	const trace = "trace-u5-6"
	sessions.Mark(trace)
	b.SetSessionTaint(sessions)

	b.SendNotification(context.Background(), trace, "merhaba")
	gate.mu.Lock()
	calls := append([]egress.SessionInfo(nil), gate.calls...)
	gate.mu.Unlock()
	if len(calls) != 1 || calls[0].SessionID != trace || calls[0].TraceID != trace {
		t.Fatalf("egress.Check calls = %+v, want exactly one keyed on %q", calls, trace)
	}
	assertGenericNotice(t, sender, trace, 1)
}

// TestProductionTelegramTextsPassContentGate (FIX-U-5 false-positive
// guard): the fixed texts this package sends for ordinary events - the
// governor alarms as formatAlarmText renders them, card notices with a real
// 64-hex pending id, the ritual question frame - never trip the content
// gate, so an unmarked session's ordinary notifications are not all
// replaced by the redacted notice.
func TestProductionTelegramTextsPassContentGate(t *testing.T) {
	const trace = "0123456789abcdef0123456789abcdef"
	const id = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	texts := []string{
		formatAlarmText(kindTaskPausedBudget, anthproxy.MsgTaskCeiling, trace, nil),
		formatAlarmText(kindBudgetDowngradeOn, anthproxy.MsgDailyBudgetBlock, trace, nil),
		formatAlarmText(kindSpendAlarm100, anthproxy.MsgMonthlyBudgetBlock, trace, nil),
		formatAlarmText(kindCacheHitAlarm, "Gunluk cache-hit orani esigin altina dustu.", trace, map[string]any{"pct": 42}),
		formatAlarmText(kindSpendAlarm80, "Gunluk harcama %80 esigini asti ($4.00 / $5.00).", trace, nil),
		fmt.Sprintf(msgW3WaitFmt, "mail_send", id),
		fmt.Sprintf(msgIrreversibleWaitFmt, policy.ClassW2, "shell_docker", id),
		fmt.Sprintf(msgCardTooLargeFmt, "fs_write: ~/Belgeler/rapor.md", 12, id),
		fmt.Sprintf(msgCardIncompleteFmt, id),
		fmt.Sprintf(msgRitualQuestionFmt, "Emre pazartesileri yüzme kursuna gider"),
		fmt.Sprintf(msgOutboundRedactedFmt, trace),
		redactedNoticeText("fs_write"),
		msgTelegramOffline,
	}
	for _, text := range texts {
		if secret, reason := outboundSecretLane(text); secret {
			t.Errorf("fixed Telegram text %q trips the content gate (%s)", text, reason)
		}
	}
}
