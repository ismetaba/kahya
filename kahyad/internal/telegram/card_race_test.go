package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"

	tele "gopkg.in/telebot.v4"

	"kahya/kahyad/internal/policy"
)

// FIX-FINAL flake-r2-tux3-card-send-race: main.go sends the Telegram card
// (`go tgBot.OnPendingApproval`) and pops the Hammerspoon card at the same
// moment. A decision made locally while the card was still being sent
// found no card in b.cards (OnApprovalDecided / OnApprovalInvalidated are
// no-ops then), and the card landed afterwards with live buttons: a later
// tap answered "Onay süresi doldu" and stamped "⏰ Süresi doldu" over an
// action that was approved and executed - the symptom TUX-3 removed. The
// card is now reconciled with the engine's recorded decision right after
// it is stored. These tests hold the keyboard chunk's send on a gate, so
// the decision lands deterministically while it is in flight (no sleeps).

// gatedSender holds the keyboard chunk's Send until release is closed and
// reports, through entered, that the send is in flight.
type gatedSender struct {
	*fakeSender
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedSender() *gatedSender {
	return &gatedSender{fakeSender: &fakeSender{}, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedSender) Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	for _, o := range opts {
		if _, ok := o.(*tele.ReplyMarkup); ok {
			g.once.Do(func() { close(g.entered) })
			<-g.release
		}
	}
	return g.fakeSender.Send(to, what, opts...)
}

// raceFixture wires the bot's hooks into the engine synchronously (the
// decision lands while the card's send is held, so the hooks find no card
// - exactly the production race) and mints one W2 fs_write pending id.
func raceFixture(t *testing.T, name string) (*Bot, *gatedSender, policyFixture, policy.PendingApprovalInfo) {
	t.Helper()
	sender := newGatedSender()
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	fix.Engine.SetApprovalDecidedHook(func(id string, approved bool) { b.OnApprovalDecided(context.Background(), id, approved) })
	fix.Engine.SetInvalidatedHook(func(info policy.PendingApprovalInfo) { b.OnApprovalInvalidated(context.Background(), info) })
	toolInput := fsWriteInput(t, name, "icerik "+name)
	trace := "trace-race-" + name
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{Tool: "fs_write", TaskID: "task-race-" + name, TraceID: trace, ToolInput: toolInput})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	return b, sender, fix, policy.PendingApprovalInfo{ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: trace}
}

// sendDuring sends info's card and runs decide while the keyboard chunk's
// send is in flight; it returns once the card send finished.
func sendDuring(t *testing.T, b *Bot, sender *gatedSender, info policy.PendingApprovalInfo, decide func()) {
	t.Helper()
	sent := make(chan struct{})
	go func() {
		b.OnPendingApproval(context.Background(), info)
		close(sent)
	}()
	<-sender.entered
	decide()
	close(sender.release)
	<-sent
}

func editTexts(s *fakeSender) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.edited))
	for i, e := range s.edited {
		out[i] = e.Text
	}
	return out
}

func TestDecisionWhileCardInFlightEditsTheLandedCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide func(t *testing.T, fix policyFixture, info policy.PendingApprovalInfo)
		check  func(text string) bool
		want   string
	}{
		{"local_approve", func(t *testing.T, fix policyFixture, info policy.PendingApprovalInfo) {
			if _, err := fix.Engine.Approve(context.Background(), info.ID, "local", ""); err != nil {
				t.Fatalf("local Approve: %v", err)
			}
		}, func(s string) bool { return strings.HasSuffix(s, suffixApproved) }, "ends " + suffixApproved},
		{"local_deny", func(t *testing.T, fix policyFixture, info policy.PendingApprovalInfo) {
			if err := fix.Engine.Deny(context.Background(), info.ID); err != nil {
				t.Fatalf("Deny: %v", err)
			}
		}, func(s string) bool { return strings.HasSuffix(s, suffixRejected) }, "ends " + suffixRejected},
		{"halt", func(t *testing.T, fix policyFixture, info policy.PendingApprovalInfo) {
			if _, err := fix.Engine.InvalidateApprovalsForTask(context.Background(), info.TraceID, "task-race-halt.md"); err != nil {
				t.Fatalf("InvalidateApprovalsForTask: %v", err)
			}
		}, func(s string) bool { return s == msgApprovalInvalidatedHalt }, "is the halt text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, sender, fix, info := raceFixture(t, tc.name+".md")
			sendDuring(t, b, sender, info, func() { tc.decide(t, fix, info) })

			edits := editTexts(sender.fakeSender)
			if len(edits) != 1 || !tc.check(edits[0]) {
				t.Fatalf("card edits = %q, want exactly one that %s", edits, tc.want)
			}
			b.mu.Lock()
			card := b.cards[info.ID]
			resolved := card != nil && card.resolved
			b.mu.Unlock()
			if !resolved {
				t.Fatal("the landed card is still live (unresolved)")
			}
			tapCard(t, b, cbActionApprove, info.ID)
			sender.mu.Lock()
			toast := sender.responded[len(sender.responded)-1]
			sender.mu.Unlock()
			if toast != msgAlreadyHandled {
				t.Errorf("a later tap answered %q, want %q", toast, msgAlreadyHandled)
			}
			for _, e := range editTexts(sender.fakeSender) {
				if strings.HasSuffix(e, suffixExpired) {
					t.Errorf("the decided card was stamped expired: %q", e)
				}
			}
		})
	}
}

// TestDecisionAfterCardStoredStillEditsOnce: the ordinary order (the card
// is stored, then the id is decided) still produces exactly one edit.
func TestDecisionAfterCardStoredStillEditsOnce(t *testing.T) {
	b, sender, fix, info := raceFixture(t, "after.md")
	close(sender.release) // never hold the send
	b.OnPendingApproval(context.Background(), info)
	if _, err := fix.Engine.Approve(context.Background(), info.ID, "local", ""); err != nil {
		t.Fatalf("local Approve: %v", err)
	}
	if edits := editTexts(sender.fakeSender); len(edits) != 1 || !strings.HasSuffix(edits[0], suffixApproved) {
		t.Fatalf("card edits = %q, want one ending %q", edits, suffixApproved)
	}
}

// TestTapAfterUnseenLocalDecisionStampsTheDecision: the id was approved
// locally but the decided hook has not reached the card yet (it runs in
// its own goroutine in production) when the user taps. The tap's own
// engine call fails (already consumed); the card is stamped with the
// recorded decision and the toast says "Zaten işlendi." - never "süresi
// doldu" over an approved action. The late hook then changes nothing.
func TestTapAfterUnseenLocalDecisionStampsTheDecision(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	var late []func()
	fix.Engine.SetApprovalDecidedHook(func(id string, approved bool) {
		late = append(late, func() { b.OnApprovalDecided(context.Background(), id, approved) })
	})
	info := sendTestCard(t, b, fix, "unseen.md")
	if _, err := fix.Engine.Approve(context.Background(), info.ID, "local", ""); err != nil {
		t.Fatalf("local Approve: %v", err)
	}
	tapCard(t, b, cbActionApprove, info.ID)
	for _, f := range late {
		f()
	}
	sender.mu.Lock()
	toast := sender.responded[len(sender.responded)-1]
	sender.mu.Unlock()
	if toast != msgAlreadyHandled {
		t.Errorf("toast = %q, want %q", toast, msgAlreadyHandled)
	}
	if edits := editTexts(sender); len(edits) != 1 || !strings.HasSuffix(edits[0], suffixApproved) {
		t.Fatalf("card edits = %q, want exactly one ending %q", edits, suffixApproved)
	}
}
