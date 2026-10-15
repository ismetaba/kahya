package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/policy"
)

// B-11 Telegram half (HANDOFF §4: "Telegram botu (uzaktan onay, yalnız
// geri-alınabilir eylemler)"): a W1/W2 card carries the Onayla/Reddet
// keyboard only for a policy.yaml reversible: true tool; every other W1/W2
// gets the keyboard-less msgIrreversibleWaitFmt notice. The engine half
// (policy.ErrNonLocalRequiresReversible) is pinned by
// TestForgedIrreversibleW2CallbackRejectedAtEngine and the policy package.

// mintPending runs a real engine Check for (tool, toolInput) and returns the
// pending approval info OnPendingApproval would receive for class.
func mintPending(t *testing.T, fix policyFixture, tool string, class policy.ActionClass, toolInput []byte, traceID string) policy.PendingApprovalInfo {
	t.Helper()
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: tool, TaskID: "task-" + traceID, TraceID: traceID, ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(%s) = %+v, %v; want NEEDS_APPROVAL", tool, d, err)
	}
	return policy.PendingApprovalInfo{ID: d.PendingApprovalID, Tool: tool, Class: class, ToolInput: toolInput, TraceID: traceID}
}

// assertIrreversibleNotice: exactly one message, byte-exact
// msgIrreversibleWaitFmt for (class, summary, id), no keyboard, no card
// tracked, one telegram_irreversible_notice ledger row.
func assertIrreversibleNotice(t *testing.T, b *Bot, sender *fakeSender, ledger *fakeLedger, info policy.PendingApprovalInfo, summary string) {
	t.Helper()
	if len(sender.sent) != 1 {
		t.Fatalf("sent = %q, want exactly 1 notice", sender.allTexts())
	}
	if sender.sent[0].Markup != nil {
		t.Fatalf("irreversible %s got an inline keyboard: %+v", info.Tool, sender.sent[0].Markup)
	}
	want := fmt.Sprintf(msgIrreversibleWaitFmt, info.Class, summary, info.ID)
	if sender.sent[0].Text != want {
		t.Fatalf("notice text = %q, want %q", sender.sent[0].Text, want)
	}
	if n := cardCount(b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0 (a notice is never a card)", n)
	}
	if ledger != nil {
		if n := ledger.countKind("telegram_irreversible_notice"); n != 1 {
			t.Fatalf("telegram_irreversible_notice rows = %d, want 1", n)
		}
	}
}

// TestIrreversibleW2NoticeOnlyNoKeyboard (B-11): an irreversible W2
// (shell_docker) pending approval gets ONE keyboard-less notice naming the
// class, the tool and the pending id - never the approve card.
func TestIrreversibleW2NoticeOnlyNoKeyboard(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)
	b.SetToolRules(fix.Policy.ToolsByName)

	info := mintPending(t, fix, "shell_docker", policy.ClassW2, []byte("echo hi\n"), "trace-b11-irrev")
	b.OnPendingApproval(context.Background(), info)

	assertIrreversibleNotice(t, b, sender, ledger, info, "shell_docker")
	if want := "⏳ Yerelde onay bekleniyor (geri alınamaz W2): shell_docker. Terminalden 'kahya approve " + info.ID + "' çalıştırın."; sender.sent[0].Text != want {
		t.Fatalf("notice text = %q, want byte-exact %q", sender.sent[0].Text, want)
	}
	if strings.Contains(sender.sent[0].Text, "echo hi") {
		t.Fatalf("the notice must not carry the script body: %q", sender.sent[0].Text)
	}
}

// TestIrreversibleW2LargeBodyIsOneNoticeNotChunks (B-11): the notice
// replaces the card entirely - a script that would need dozens of chunks
// as a card is still exactly one notice (no chunk storm, no too-large
// notice, nothing tracked).
func TestIrreversibleW2LargeBodyIsOneNoticeNotChunks(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	info := mintPending(t, fix, "shell_docker", policy.ClassW2, []byte(fillerLines(2000)), "trace-b11-irrev-large")
	b.OnPendingApproval(context.Background(), info)

	assertIrreversibleNotice(t, b, sender, ledger, info, "shell_docker")
	if n := ledger.countKind("telegram_card_too_large"); n != 0 {
		t.Fatalf("telegram_card_too_large rows = %d, want 0 (no card was attempted)", n)
	}
}

// TestReversibleW2StillGetsCard (B-11): a reversible: true W2 (the
// fixture's fs_write) keeps the byte-exact card with the two-button
// keyboard on its final chunk.
func TestReversibleW2StillGetsCard(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)
	b.SetToolRules(fix.Policy.ToolsByName)

	info := mintPending(t, fix, "fs_write", policy.ClassW2, fsWriteInput(t, "~/Belgeler/rapor.md", "yeni satır"), "trace-b11-rev")
	b.OnPendingApproval(context.Background(), info)

	if len(sender.sent) == 0 {
		t.Fatal("no card sent for a reversible W2")
	}
	last := sender.sent[len(sender.sent)-1]
	if last.Markup == nil || len(last.Markup.InlineKeyboard) == 0 || len(last.Markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("final chunk keyboard = %+v, want Onayla/Reddet", last.Markup)
	}
	if !strings.Contains(strings.Join(sender.allTexts(), ""), "yeni satır") {
		t.Fatalf("card does not carry the diff: %q", sender.allTexts())
	}
	if n := ledger.countKind("telegram_irreversible_notice"); n != 0 {
		t.Fatalf("telegram_irreversible_notice rows = %d, want 0", n)
	}
	if n := cardCount(b); n != 1 {
		t.Fatalf("tracked cards = %d, want 1", n)
	}
}

// TestUnwiredToolRulesTreatedIrreversible (B-11, fail-closed): with no
// tool rules wired, even a W1 reversible-in-policy tool gets the notice -
// never a keyboard by omission.
func TestUnwiredToolRulesTreatedIrreversible(t *testing.T) {
	sender, ledger := &fakeSender{}, &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)
	b.SetToolRules(nil)

	input, err := json.Marshal(map[string]string{"file": "inbox/not.md", "content": "süt al"})
	if err != nil {
		t.Fatal(err)
	}
	info := mintPending(t, fix, "memory_write", policy.ClassW1, input, "trace-b11-unwired")
	b.OnPendingApproval(context.Background(), info)

	assertIrreversibleNotice(t, b, sender, ledger, info, "memory_write")
}

// TestSecretLaneIrreversibleStillRedacted (B-11 after P-6, G00 C-4): the
// reversible/irreversible branch runs AFTER both redaction gates, so a
// secret-lane irreversible action gets only the redacted title - not the
// irreversible notice (whose summary may carry a path) and never a card.
func TestSecretLaneIrreversibleStillRedacted(t *testing.T) {
	t.Run("glob_path", func(t *testing.T) {
		r := newRedactFixture(t)
		rules := map[string]policy.ToolRule{}
		for name, rule := range testToolRules {
			rules[name] = rule
		}
		fsw := rules["fs_write"]
		fsw.Reversible = false
		rules["fs_write"] = fsw
		r.b.SetToolRules(rules)
		r.deliver(t, "fs_write", policy.ClassW2, fsWriteInput(t, filepath.Join(r.home, "Finans", "butce.md"), "kira"), "trace-b11-secret-glob")
		assertOnlyRedactedTitle(t, r, "fs_write", []string{"Finans", "butce", "kira", "geri alınamaz"}, "glob")
	})
	t.Run("content", func(t *testing.T) {
		r := newRedactFixture(t)
		r.deliver(t, "shell_docker", policy.ClassW2, []byte("echo "+testIBAN+"\n"), "trace-b11-secret-content")
		assertOnlyRedactedTitle(t, r, "shell_docker", []string{"TR33", "8413", "geri alınamaz"}, "content:iban")
	})
}

// TestProductionPolicyTelegramKeyboardOnlyForReversibleTools (B-11
// acceptance, production policy.yaml): every irreversible W1/W2 (today all
// the W2s) gets the keyboard-less notice; every reversible one (today the
// W1s, incl. mail_draft - decision G01-3) gets the approve card; W3
// (mail_send) keeps its own notice.
func TestProductionPolicyTelegramKeyboardOnlyForReversibleTools(t *testing.T) {
	pol, err := policy.Load(filepath.Join("..", "..", "..", "policy.yaml"))
	if err != nil {
		t.Fatalf("policy.Load(policy.yaml): %v", err)
	}
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var w1, w2 int
	for _, rule := range pol.Tools {
		if rule.Class == policy.ClassR {
			continue
		}
		t.Run(rule.Name, func(t *testing.T) {
			sender := &fakeSender{}
			b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
			b.SetToolRules(pol.ToolsByName)
			input := []byte("ornek govde")
			if rule.Name == "fs_write" || rule.Name == "fs_delete" {
				input = fsWriteInput(t, "~/Belgeler/ornek.md", "ornek")
			}
			b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
				ID: id, Tool: rule.Name, Class: rule.Class, ToolInput: input, TraceID: "trace-b11-prod-" + rule.Name,
			})
			if len(sender.sent) == 0 {
				t.Fatal("nothing sent")
			}
			last := sender.sent[len(sender.sent)-1]
			switch {
			case rule.Class == policy.ClassW3:
				if last.Markup != nil || !strings.HasPrefix(last.Text, "⏳ Yerelde onay bekleniyor (W3): ") {
					t.Fatalf("W3 %s = %+v, want the keyboard-less W3 notice", rule.Name, last)
				}
			case rule.Reversible:
				if last.Markup == nil {
					t.Fatalf("reversible %s %s got no keyboard: %q", rule.Class, rule.Name, last.Text)
				}
			default:
				if len(sender.sent) != 1 || last.Markup != nil || !strings.HasPrefix(last.Text, "⏳ Yerelde onay bekleniyor (geri alınamaz "+string(rule.Class)+"): ") {
					t.Fatalf("irreversible %s %s = %+v, want one keyboard-less notice", rule.Class, rule.Name, sender.sent)
				}
			}
		})
		switch rule.Class {
		case policy.ClassW1:
			w1++
		case policy.ClassW2:
			w2++
		}
	}
	if w1 == 0 || w2 == 0 {
		t.Fatalf("policy.yaml W1=%d W2=%d tools, want both classes covered", w1, w2)
	}
	// The B-11 acceptance lists, by name: these are notice-only from
	// Telegram, these keep the approve card (G01-3: mail_draft stays
	// Telegram-approvable). Making one of them reversible (or not) is a
	// deliberate product decision that must update this list.
	for _, name := range []string{"shell_docker", "shell_host", "applescript_run", "jxa_run", "shortcuts_run", "telegram_send"} {
		if rule, ok := pol.ToolsByName[name]; !ok || rule.Reversible {
			t.Errorf("policy.yaml %s: present=%v reversible=%v, want an irreversible tool (notice-only on Telegram)", name, ok, rule.Reversible)
		}
	}
	for _, name := range []string{"memory_write", "memory_forget", "fs_write", "fs_delete", "mail_draft"} {
		if rule, ok := pol.ToolsByName[name]; !ok || !rule.Reversible {
			t.Errorf("policy.yaml %s: present=%v reversible=%v, want a reversible tool (Telegram card)", name, ok, rule.Reversible)
		}
	}
}
