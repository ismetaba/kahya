package telegram

import (
	"context"
	"strings"
	"testing"
)

// TestTelegramTapEditsCardOnce (FIX-FINAL flake-r2-telegram-tap-double-edit):
// a Telegram tap drives Engine.Approve/Deny, which fires the decided hook
// (main.go runs it as `go tgBot.OnApprovalDecided`), and handleCallback then
// answers the callback and resolves the card itself. When the hook got to
// the card first, markResolved edited it a second time (an identical
// editMessageText Telegram answers 400 "message is not modified", plus a
// second egress check and byte charge). Whichever of the two resolves the
// card first is now the only one that edits it. The hook order is forced
// deterministically: run inside the engine call (hook first) or after the
// tap returned (tap first).
func TestTelegramTapEditsCardOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		action    byte
		suffix    string
		hookFirst bool
	}{
		{"approve_hook_first", cbActionApprove, suffixApproved, true},
		{"approve_tap_first", cbActionApprove, suffixApproved, false},
		{"deny_hook_first", cbActionDeny, suffixRejected, true},
		{"deny_tap_first", cbActionDeny, suffixRejected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{}
			fix := newPolicyFixture(t)
			b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
			var late []func()
			fix.Engine.SetApprovalDecidedHook(func(id string, approved bool) {
				if tc.hookFirst {
					b.OnApprovalDecided(context.Background(), id, approved)
					return
				}
				late = append(late, func() { b.OnApprovalDecided(context.Background(), id, approved) })
			})
			info := sendTestCard(t, b, fix, tc.name+".md")
			tapCard(t, b, tc.action, info.ID)
			for _, f := range late {
				f()
			}
			edits := editTexts(sender)
			if len(edits) != 1 || !strings.HasSuffix(edits[0], tc.suffix) {
				t.Fatalf("one tap produced card edits %q, want exactly one ending %q", edits, tc.suffix)
			}
		})
	}
}

// TestFailedTapNeverOverwritesADecidedCard: the decided hook already stamped
// the card (another surface approved first) when a tap's engine call
// fails; markResolved must not overwrite the decision with "Süresi doldu".
func TestFailedTapNeverOverwritesADecidedCard(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	info := sendTestCard(t, b, fix, "overwrite.md")
	if _, err := fix.Engine.Approve(context.Background(), info.ID, "local", ""); err != nil {
		t.Fatalf("local Approve: %v", err)
	}
	// The decided hook lands while the tap is between its resolved check
	// and its own markResolved: model it by resolving through the hook and
	// then calling markResolved the way a failed tap does.
	b.OnApprovalDecided(context.Background(), info.ID, true)
	b.markResolved(info.ID, suffixExpired)
	edits := editTexts(sender)
	if len(edits) != 1 || !strings.HasSuffix(edits[0], suffixApproved) {
		t.Fatalf("card edits = %q, want only the approval stamp", edits)
	}
}
