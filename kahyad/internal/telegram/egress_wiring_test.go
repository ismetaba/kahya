// egress_wiring_test.go covers MINOR C (b.respond bypassed egress.Check)
// and MINOR D (every egress.Check in this package left SessionInfo.
// SessionID empty, so the sensitive-read rule could never apply to a
// Telegram send) - both from the same review pass, and both about HOW
// this package's outbound calls thread through kahyad/internal/egress.Gate,
// so they live together here rather than bloating bot_test.go/
// approvals_test.go further.
package telegram

import (
	"context"
	"testing"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
)

// ---- MINOR C: b.respond must be egress-checked like every other send ----

// TestRespondCallsEgressCheck proves b.respond now goes through
// EgressGate.Check at all (before the fix it called b.sender.Respond
// directly, the one outbound path in this package that skipped the gate).
func TestRespondCallsEgressCheck(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.respond(&tele.Callback{}, "trace-respond-ok", toastApproved)

	if len(gate.calls) != 1 {
		t.Fatalf("egress.Check calls = %d, want 1 (respond must be gated)", len(gate.calls))
	}
	if len(sender.responded) != 1 || sender.responded[0] != toastApproved {
		t.Fatalf("responded = %v, want [%q] (an ALLOW decision must still let the toast through)", sender.responded, toastApproved)
	}
}

// TestRespondDegradesGracefullyWhenEgressBlocks is MINOR C's own
// regression test, verbatim per the task spec: a blocked egress decision
// must make the toast a silent no-op (never an error, never a panic) -
// "the toast is non-critical" - since the approval card itself (editCard)
// already carries the terminal state the user needs to see.
func TestRespondDegradesGracefullyWhenEgressBlocks(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: false, Reason: "denied for test", Rule: "egress_blocked_allowlist"}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.respond(&tele.Callback{}, "trace-respond-blocked", toastApproved)

	if len(gate.calls) != 1 {
		t.Fatalf("egress.Check calls = %d, want 1 (still checked, even though it will deny)", len(gate.calls))
	}
	if len(sender.responded) != 0 {
		t.Fatalf("responded = %v, want none (a blocked toast must degrade gracefully, never reach the sender)", sender.responded)
	}
}

// TestRespondDegradesGracefullyWhenEgressErrors covers the OTHER half of
// "fail-closed" for a non-critical toast: a Gate-internal error (not just
// Allow=false) must ALSO be treated as blocked, never as a green light to
// proceed.
func TestRespondDegradesGracefullyWhenEgressErrors(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{err: context.DeadlineExceeded}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.respond(&tele.Callback{}, "trace-respond-erroring", toastApproved)

	if len(sender.responded) != 0 {
		t.Fatalf("responded = %v, want none (a Gate error must degrade gracefully, not send)", sender.responded)
	}
}

// ---- MINOR D: SessionInfo.SessionID must be populated (== traceID) ----

// TestSendPopulatesSessionID proves b.send threads SessionID == traceID
// into every egress.Check call, not just TraceID - before the fix,
// SessionID was always left empty, so the sensitive-read rule
// (egress.Gate.Check's decision order step 1) could never apply to a
// Telegram send no matter what.
func TestSendPopulatesSessionID(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.send(context.Background(), "trace-send-session", "hello", nil)

	if len(gate.calls) != 1 {
		t.Fatalf("egress.Check calls = %d, want 1", len(gate.calls))
	}
	got := gate.calls[0]
	if got.SessionID != "trace-send-session" {
		t.Errorf("SessionInfo.SessionID = %q, want %q (MINOR D)", got.SessionID, "trace-send-session")
	}
	if got.TraceID != "trace-send-session" {
		t.Errorf("SessionInfo.TraceID = %q, want %q", got.TraceID, "trace-send-session")
	}
}

// TestEditCardPopulatesSessionID is send's counterpart for editCard.
func TestEditCardPopulatesSessionID(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	card := &cardState{ChatID: testChatID, MessageID: 1, Text: "orijinal metin", TraceID: "trace-editcard-session"}
	b.editCard(card, suffixApproved)

	if len(gate.calls) != 1 {
		t.Fatalf("egress.Check calls = %d, want 1", len(gate.calls))
	}
	got := gate.calls[0]
	if got.SessionID != "trace-editcard-session" {
		t.Errorf("SessionInfo.SessionID = %q, want %q (MINOR D)", got.SessionID, "trace-editcard-session")
	}
	if got.TraceID != "trace-editcard-session" {
		t.Errorf("SessionInfo.TraceID = %q, want %q", got.TraceID, "trace-editcard-session")
	}
}

// TestRespondPopulatesSessionID is send/editCard's counterpart for
// respond - the traceID handleCallback resolves from the card is what
// respond must forward into SessionInfo (both fields, MINOR D).
func TestRespondPopulatesSessionID(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.respond(&tele.Callback{}, "trace-respond-session", toastRejected)

	if len(gate.calls) != 1 {
		t.Fatalf("egress.Check calls = %d, want 1", len(gate.calls))
	}
	got := gate.calls[0]
	if got.SessionID != "trace-respond-session" {
		t.Errorf("SessionInfo.SessionID = %q, want %q (MINOR D)", got.SessionID, "trace-respond-session")
	}
	if got.TraceID != "trace-respond-session" {
		t.Errorf("SessionInfo.TraceID = %q, want %q", got.TraceID, "trace-respond-session")
	}
}

// TestHandleCallbackRespondsWithCardTraceID is an end-to-end check that
// handleCallback actually resolves traceID from the OPEN card (rather than
// leaving it empty) before calling respond - drives a real W2
// approve-callback round trip through a fakeEgressGate so the SessionInfo
// every respond() call receives can be inspected directly.
func TestHandleCallbackRespondsWithCardTraceID(t *testing.T) {
	sender := &fakeSender{}
	gate := &fakeEgressGate{decision: egress.Decision{Allow: true}}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, gate, fix.Engine, nil)

	const traceID = "trace-callback-session"
	toolInput := fsWriteInput(t, "notlar-session.md", "icerik")
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-session", TraceID: traceID, ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: traceID,
	})
	if len(sender.sent) == 0 {
		t.Fatal("no approval card sent")
	}

	approveData, err := encodeCallbackData(cbActionApprove, d.PendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, approveData))

	if len(sender.responded) != 1 {
		t.Fatalf("responded count = %d, want 1", len(sender.responded))
	}
	// Every egress.Check call this whole round trip makes - the original
	// card send(s), respond()'s toast, AND markResolved's editCard - must
	// carry the SAME traceID as the pending approval; none of them may be
	// empty (which is what the pre-fix code always sent for SessionID).
	if len(gate.calls) < 3 {
		t.Fatalf("egress.Check calls = %d, want at least 3 (card send + toast + card edit)", len(gate.calls))
	}
	for i, call := range gate.calls {
		if call.SessionID != traceID || call.TraceID != traceID {
			t.Errorf("egress.Check call %d SessionInfo = %+v, want SessionID=TraceID=%q", i, call, traceID)
		}
	}
}

// ---- MINOR D, end-to-end: the real Gate + SensitiveTracker mechanism ----

// TestSendBlockedForSensitiveSessionWhenHostNotAllowlisted proves the
// SessionID wiring isn't just cosmetic: against a REAL *egress.Gate and
// *egress.SensitiveTracker (no fakes) with api.telegram.org deliberately
// NOT in the allowlist, marking a trace sensitive must make Check's DENY
// reason switch from egress_blocked_allowlist to egress_blocked_sensitive
// - the more specific rule can only ever fire if SessionID actually
// reaches Check (an empty SessionID makes `sensitive` structurally always
// false, per Gate.Check's own decision order).
func TestSendBlockedForSensitiveSessionWhenHostNotAllowlisted(t *testing.T) {
	sender := &fakeSender{}
	gateLedger := &fakeLedger{}
	sessions := egress.NewSensitiveTracker()
	const traceID = "trace-sensitive-send"
	sessions.Mark(traceID)

	// Deliberately an EMPTY allowlist (api.telegram.org not present) -
	// isolates the sensitive-session rule as the ONLY thing that could
	// ever produce egress_blocked_sensitive instead of the plain
	// egress_blocked_allowlist every other unmatched host gets.
	gate := egress.NewGate(policy.EgressConfig{}, sessions, nil, gateLedger, nil, nil)
	b := newTestBot(t, testConfig(), sender, nil, gate, nil, nil)

	b.send(context.Background(), traceID, "hello", nil)

	if len(sender.sent) != 0 {
		t.Fatalf("sent count = %d, want 0 (host is not allowlisted either way)", len(sender.sent))
	}
	if n := gateLedger.countKind(egress.EventBlockedSensitive); n != 1 {
		t.Fatalf("egress_blocked_sensitive ledger rows = %d, want 1 - proves send() threads SessionID: traceID into egress.Check (a bare TraceID-only SessionInfo could never trigger this rule, and would instead log egress_blocked_allowlist)", n)
	}
	if n := gateLedger.countKind(egress.EventBlockedAllowlist); n != 0 {
		t.Fatalf("egress_blocked_allowlist ledger rows = %d, want 0 - the sensitive rule must take priority once SessionID is wired", n)
	}
}
