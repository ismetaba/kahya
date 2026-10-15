package telegram

import (
	"context"
	"strings"
	"testing"

	"kahya/kahyad/internal/policy"
)

func lastEdit(t *testing.T, s *fakeSender) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.edited) == 0 {
		t.Fatal("the card was never edited")
	}
	return s.edited[len(s.edited)-1].Text
}

func editCount(s *fakeSender) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.edited)
}

// TestLocalApproveEditsTelegramCard (FIX-FINAL TUX-3): a card stayed live
// after the same id was approved locally (kahya approve / Hammerspoon), so
// a later tap answered "Onay süresi doldu, yeniden isteyin." and stamped
// "Süresi doldu" over an action that was approved and executed. The
// decided hook now edits the card to the decision; a later tap answers
// "Zaten işlendi." and decides nothing.
func TestLocalApproveEditsTelegramCard(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	info := sendTestCard(t, b, fix, "local-ok.md")
	before := editCount(sender)
	if _, err := fix.Engine.Approve(context.Background(), info.ID, "local", ""); err != nil {
		t.Fatalf("local Approve: %v", err)
	}
	b.OnApprovalDecided(context.Background(), info.ID, true)
	if n := editCount(sender) - before; n != 1 || !strings.HasSuffix(lastEdit(t, sender), suffixApproved) {
		t.Fatalf("edits = %d, last %q; want one ending %q", n, lastEdit(t, sender), suffixApproved)
	}
	tapCard(t, b, cbActionApprove, info.ID)
	sender.mu.Lock()
	toast := sender.responded[len(sender.responded)-1]
	sender.mu.Unlock()
	if toast != msgAlreadyHandled {
		t.Fatalf("toast after a stale tap = %q, want %q", toast, msgAlreadyHandled)
	}
	if strings.Contains(lastEdit(t, sender), suffixExpired) {
		t.Fatal("the decided card was stamped expired")
	}
	if n := countLedgerRows(t, fix, "policy_feedback_approved"); n != 1 {
		t.Fatalf("policy_feedback_approved rows = %d, want 1 (the local one)", n)
	}
}

// TestLocalDenyEditsTelegramCard: a local deny edits the card to the
// rejection suffix.
func TestLocalDenyEditsTelegramCard(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	info := sendTestCard(t, b, fix, "local-no.md")
	if err := fix.Engine.Deny(context.Background(), info.ID); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	b.OnApprovalDecided(context.Background(), info.ID, false)
	if got := lastEdit(t, sender); !strings.HasSuffix(got, suffixRejected) {
		t.Fatalf("card = %q, want it ending %q", got, suffixRejected)
	}
}

// TestHaltTextNotOverwrittenByDecidedHook: a halt fires the decided hook
// with approved=false after invalidating the id; whichever handler runs
// first, the card ends on the byte-exact halt text, never "Reddedildi".
func TestHaltTextNotOverwrittenByDecidedHook(t *testing.T) {
	for _, decidedFirst := range []bool{true, false} {
		sender := &fakeSender{}
		fix := newPolicyFixture(t)
		b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
		info := sendTestCard(t, b, fix, "halt.md")
		if _, err := fix.Engine.InvalidateApprovalsForTask(context.Background(), info.TraceID, "task-card-halt.md"); err != nil {
			t.Fatalf("InvalidateApprovalsForTask: %v", err)
		}
		if decidedFirst {
			b.OnApprovalDecided(context.Background(), info.ID, false)
			b.OnApprovalInvalidated(context.Background(), policy.PendingApprovalInfo{ID: info.ID})
		} else {
			b.OnApprovalInvalidated(context.Background(), policy.PendingApprovalInfo{ID: info.ID})
			b.OnApprovalDecided(context.Background(), info.ID, false)
		}
		if got := lastEdit(t, sender); got != msgApprovalInvalidatedHalt {
			t.Fatalf("decidedFirst=%v: card = %q, want the halt text", decidedFirst, got)
		}
		sender.mu.Lock()
		for _, e := range sender.edited {
			if strings.HasSuffix(e.Text, suffixRejected) {
				t.Errorf("decidedFirst=%v: the halted card was edited to %q", decidedFirst, e.Text)
			}
		}
		sender.mu.Unlock()
	}
}
