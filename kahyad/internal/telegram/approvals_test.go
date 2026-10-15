package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/approval"
	"kahya/kahyad/internal/policy"
)

// fsWriteInput builds the {"path","content_base64"} envelope fs_write's
// tool_input actually carries (mirrors mcp/fs's own toolInputEnvelope).
func fsWriteInput(t *testing.T, path, content string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"path":           path,
		"content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		t.Fatalf("marshal fs_write input: %v", err)
	}
	return b
}

// TestW2ApprovalCardByteExactDiff is the task spec's own acceptance
// criterion, verbatim: "W2 card contains byte-exact diff chunks (fixture
// with Turkish content `Bütçe raporu ği üşç.md` survives byte-exact)".
func TestW2ApprovalCardByteExactDiff(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	const turkishName = "Bütçe raporu ği üşç.md"
	const content = "yeni içerik: ğüşıöç"
	toolInput := fsWriteInput(t, turkishName, content)

	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-1", TraceID: "trace-w2-diff", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(fs_write) = %+v, want NEEDS_APPROVAL", d)
	}

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2,
		ToolInput: toolInput, TraceID: "trace-w2-diff",
	})

	if len(sender.sent) == 0 {
		t.Fatalf("no message sent for a W2 approval card")
	}
	full := strings.Join(sender.allTexts(), "")
	if !strings.Contains(full, turkishName) {
		t.Errorf("sent text does not contain the byte-exact Turkish filename %q; got:\n%s", turkishName, full)
	}
	if !strings.Contains(full, content) {
		t.Errorf("sent text does not contain the byte-exact content %q; got:\n%s", content, full)
	}

	// The keyboard must be attached to the LAST chunk only.
	last := sender.sent[len(sender.sent)-1]
	if last.Markup == nil || len(last.Markup.InlineKeyboard) == 0 {
		t.Fatalf("final chunk has no inline keyboard: %+v", last)
	}
	for i := 0; i < len(sender.sent)-1; i++ {
		if sender.sent[i].Markup != nil {
			t.Errorf("chunk %d unexpectedly carries a keyboard - only the final chunk should", i)
		}
	}
	btns := last.Markup.InlineKeyboard[0]
	if len(btns) != 2 || btns[0].Text != btnOnayla || btns[1].Text != btnReddet {
		t.Errorf("keyboard buttons = %+v, want [%q %q]", btns, btnOnayla, btnReddet)
	}
	// Callback data must never exceed Telegram's 64-byte limit and must
	// never contain any payload content (only the encoded id).
	for _, btn := range btns {
		if len(btn.Data) > 64 {
			t.Errorf("callback_data %q is %d bytes, want <= 64", btn.Data, len(btn.Data))
		}
		if strings.Contains(btn.Data, turkishName) || strings.Contains(btn.Data, content) {
			t.Errorf("callback_data leaked payload content: %q", btn.Data)
		}
	}
}

// fillerLines is n lines of clean filler text (the deterministic
// secret-lane pre-pass finds nothing in it).
func fillerLines(n int) string {
	var big strings.Builder
	for i := 0; i < n; i++ {
		big.WriteString("line of filler content to force chunking across multiple telegram messages\n")
	}
	return big.String()
}

// TestW2ApprovalCardMultiChunkKeyboardOnlyOnLast forces a diff large
// enough to require multiple Telegram messages (150 lines, 3-4 chunks,
// within maxCardChunks - FIX-P-6) and asserts the keyboard is attached to
// ONLY the final one.
func TestW2ApprovalCardMultiChunkKeyboardOnlyOnLast(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)

	toolInput := fsWriteInput(t, "buyuk-dosya.txt", fillerLines(150))

	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-2", TraceID: "trace-w2-multi", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-w2-multi",
	})

	if len(sender.sent) < 2 {
		t.Fatalf("expected multiple chunks for a large diff, got %d message(s)", len(sender.sent))
	}
	for i, s := range sender.sent[:len(sender.sent)-1] {
		if s.Markup != nil {
			t.Errorf("chunk %d/%d unexpectedly carries a keyboard", i, len(sender.sent))
		}
		if len([]rune(s.Text)) > 4096 {
			t.Errorf("chunk %d exceeds the 4096-rune Telegram limit: %d runes", i, len([]rune(s.Text)))
		}
	}
	last := sender.sent[len(sender.sent)-1]
	if last.Markup == nil {
		t.Fatal("final chunk missing its inline keyboard")
	}
}

// TestApprovalCallbackApprovesAndEditsCard drives a full end-to-end W2
// approve: mint via the real engine, deliver the card, tap "✅ Onayla" via
// a forged-but-allowlisted callback update, and assert the engine actually
// approved it (token minted, ledgered `remote`) and the card was edited to
// its terminal state.
func TestApprovalCallbackApprovesAndEditsCard(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	toolInput := fsWriteInput(t, "notlar.md", "merhaba dunya")
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-3", TraceID: "trace-approve", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-approve",
	})
	if len(sender.sent) == 0 {
		t.Fatal("no approval card sent")
	}

	approveData, err := encodeCallbackData(cbActionApprove, d.PendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, approveData))

	if len(sender.responded) != 1 || sender.responded[0] != toastApproved {
		t.Fatalf("responded = %v, want [%q]", sender.responded, toastApproved)
	}
	if len(sender.edited) != 1 {
		t.Fatalf("edited count = %d, want 1", len(sender.edited))
	}
	if !strings.HasSuffix(sender.edited[0].Text, suffixApproved) {
		t.Errorf("edited text = %q, want suffix %q", sender.edited[0].Text, suffixApproved)
	}

	// The engine really did approve it (surface=remote implied by
	// surface="telegram" passed to Approve - policy_feedback_approved is
	// ledgered with surface:"telegram").
	found := false
	for _, ev := range fixtureLedgerEvents(t, fix, "policy_feedback_approved") {
		if ev["surface"] == "telegram" {
			found = true
		}
	}
	if !found {
		t.Error("no policy_feedback_approved event with surface=telegram found in the real engine's ledger")
	}

	// A second (duplicate/redelivered) tap must be idempotent.
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, approveData))
	if len(sender.responded) != 2 || sender.responded[1] != msgAlreadyHandled {
		t.Fatalf("second tap responded = %v, want second element %q", sender.responded, msgAlreadyHandled)
	}
	if len(sender.edited) != 1 {
		t.Errorf("edited count after duplicate tap = %d, want still 1 (no second edit)", len(sender.edited))
	}
}

// TestApprovalCallbackDenies drives a full end-to-end W2 deny.
func TestApprovalCallbackDenies(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)

	toolInput := fsWriteInput(t, "notlar2.md", "icerik")
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-4", TraceID: "trace-deny", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-deny",
	})

	denyData, err := encodeCallbackData(cbActionDeny, d.PendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, denyData))

	if len(sender.responded) != 1 || sender.responded[0] != toastRejected {
		t.Fatalf("responded = %v, want [%q]", sender.responded, toastRejected)
	}
	if len(sender.edited) != 1 || !strings.HasSuffix(sender.edited[0].Text, suffixRejected) {
		t.Fatalf("edited = %+v, want suffix %q", sender.edited, suffixRejected)
	}
}

// TestW3NoticeOnlyNoKeyboard is the task spec's own acceptance criterion:
// a W3 action produces ONLY the notify message, no buttons.
func TestW3NoticeOnlyNoKeyboard(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)

	toolInput := []byte(`{"to":"a@b.com"}`)
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "mail_send", TaskID: "task-5", TraceID: "trace-w3", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(mail_send) = %+v, want NEEDS_APPROVAL (W3 always needs approval)", d)
	}

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "mail_send", Class: policy.ClassW3, ToolInput: toolInput, TraceID: "trace-w3",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 (notify-only)", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.Markup != nil {
		t.Fatalf("W3 notice must NEVER carry a keyboard, got %+v", msg.Markup)
	}
	if !strings.Contains(msg.Text, "⏳ Yerelde onay bekleniyor (W3)") {
		t.Errorf("W3 notice text = %q, want the fixed Turkish prefix", msg.Text)
	}
	if !strings.Contains(msg.Text, "kahya approve "+d.PendingApprovalID) {
		t.Errorf("W3 notice text = %q, want it to name the CLI command with the pending id", msg.Text)
	}
}

// TestForgedW3CallbackRejectedAtEngine is the task spec's own acceptance
// criterion, verbatim: "Test that a FORGED W3 callback is rejected at the
// engine (w3_nonlocal_approval_rejected backstop)". Even though this
// bot's W3 flow never attaches a keyboard, an attacker who somehow learns
// a W3 pending_approval_id and crafts their own callback data for it must
// still be rejected by the ENGINE itself, not merely by this bot's own
// restraint - Telegram approving W3 must be impossible under ANY
// configuration.
func TestForgedW3CallbackRejectedAtEngine(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	toolInput := []byte(`{"to":"victim@example.com"}`)
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "mail_send", TaskID: "task-6", TraceID: "trace-forged-w3", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(mail_send) = %+v, want NEEDS_APPROVAL", d)
	}

	// This bot never sent a keyboard for this id (W3 notify-only) - the
	// attacker hand-crafts the callback data themselves, from an otherwise
	// correctly-allowlisted chat/user (isolating the W3 backstop from the
	// separately-tested allowlist check).
	forged, err := encodeCallbackData(cbActionApprove, d.PendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, forged))

	if len(sender.responded) != 1 || sender.responded[0] != msgExpired {
		t.Fatalf("responded = %v, want [%q] (the engine's rejection surfaces as a generic failure, never a success)", sender.responded, msgExpired)
	}
	if n := countLedgerRows(t, fix, "w3_nonlocal_approval_rejected"); n != 1 {
		t.Fatalf("w3_nonlocal_approval_rejected ledger rows = %d, want 1 (the ENGINE's own backstop, not this bot's)", n)
	}

	// The SAME id must still be approvable from the LOCAL surface - the
	// rejected forged Telegram attempt must not have consumed it.
	if _, err := fix.Engine.Approve(context.Background(), d.PendingApprovalID, "local", "onayla"); err != nil {
		t.Fatalf("Approve(surface=local) after a rejected forged Telegram attempt: %v", err)
	}
}

// ---- shared ledger-row query helpers (against the REAL sqlite store) ----

func fixtureLedgerEvents(t *testing.T, fix policyFixture, kind string) []map[string]any {
	t.Helper()
	rows, err := fix.Store.Queries.ListEventsByKind(context.Background(), kind)
	if err != nil {
		t.Fatalf("ListEventsByKind(%s): %v", kind, err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var payload map[string]any
		if err := json.Unmarshal([]byte(r.Payload), &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

func countLedgerRows(t *testing.T, fix policyFixture, kind string) int {
	t.Helper()
	rows, err := fix.Store.Queries.ListEventsByKind(context.Background(), kind)
	if err != nil {
		t.Fatalf("ListEventsByKind(%s): %v", kind, err)
	}
	return len(rows)
}

// TestForgedIrreversibleW2CallbackRejectedAtEngine (B-11 engine half): a
// Telegram approve callback for an IRREVERSIBLE W2 pending approval - forged
// or from a card - is refused by the engine's own backstop
// (policy.ErrNonLocalRequiresReversible), surfaces as the generic expired
// reply, approves nothing, and leaves the id approvable locally.
func TestForgedIrreversibleW2CallbackRejectedAtEngine(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, &fakeLedger{}, newAllowGate(t), fix.Engine, nil)

	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "shell_docker", TaskID: "task-irrev", TraceID: "trace-forged-irrev", ToolInput: []byte(`{"command":"echo hi"}`),
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(shell_docker) = %+v, %v", d, err)
	}
	forged, err := encodeCallbackData(cbActionApprove, d.PendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, forged))

	if len(sender.responded) != 1 || sender.responded[0] != msgExpired {
		t.Fatalf("responded = %v, want [%q]", sender.responded, msgExpired)
	}
	if n := countLedgerRows(t, fix, "nonlocal_irreversible_approval_rejected"); n != 1 {
		t.Fatalf("nonlocal_irreversible_approval_rejected ledger rows = %d, want 1", n)
	}
	if n := countLedgerRows(t, fix, "policy_feedback_approved"); n != 0 {
		t.Fatalf("policy_feedback_approved ledger rows = %d, want 0", n)
	}
	if _, err := fix.Engine.Approve(context.Background(), d.PendingApprovalID, "local", ""); err != nil {
		t.Fatalf("Approve(surface=local) after the refused Telegram attempt: %v", err)
	}
}

// ---- FIX-P-19: the approval-card map is bounded ----

// cardClock is a settable fake clock for b.now (and, where a test needs
// the engine to agree, policy.Engine.SetClock).
type cardClock struct{ t time.Time }

func (c *cardClock) now() time.Time          { return c.t }
func (c *cardClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newCardClock() *cardClock               { return &cardClock{t: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)} }

// sendTestCard mints a real W2 fs_write pending approval through the
// engine and delivers its card; returns the pending approval info.
func sendTestCard(t *testing.T, b *Bot, fix policyFixture, name string) policy.PendingApprovalInfo {
	t.Helper()
	toolInput := fsWriteInput(t, name, "icerik "+name)
	trace := "trace-card-" + name
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-card-" + name, TraceID: trace, ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(%s) = %+v, %v", name, d, err)
	}
	info := policy.PendingApprovalInfo{ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: trace}
	b.OnPendingApproval(context.Background(), info)
	b.mu.Lock()
	_, tracked := b.cards[info.ID]
	b.mu.Unlock()
	if !tracked {
		t.Fatalf("card for %s was not tracked after a successful send", name)
	}
	return info
}

func tapCard(t *testing.T, b *Bot, action byte, id string) {
	t.Helper()
	data, err := encodeCallbackData(action, id)
	if err != nil {
		t.Fatalf("encodeCallbackData: %v", err)
	}
	b.tb.ProcessUpdate(callbackUpdate(testChatID, testUserID, data))
}

func cardCount(b *Bot) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cards)
}

// TestCardsEvictedAfterResolutionRetention (FIX-P-19): a resolved card is
// kept for cardResolvedRetention (late/duplicate taps still answer "Zaten
// işlendi.") and then evicted; the sweep is a telegram_cards_swept line.
func TestCardsEvictedAfterResolutionRetention(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	clock := newCardClock()
	b.now = clock.now
	log, lines := newJSONLTestLogger(t)
	b.log = log

	info := sendTestCard(t, b, fix, "retention.md")
	tapCard(t, b, cbActionApprove, info.ID)
	if len(sender.responded) != 1 || sender.responded[0] != toastApproved {
		t.Fatalf("responded = %v, want [%q]", sender.responded, toastApproved)
	}

	clock.advance(cardResolvedRetention - time.Minute)
	if n := b.sweepCards(clock.now()); n != 0 || cardCount(b) != 1 {
		t.Fatalf("sweep at retention-1m evicted %d (left %d), want 0 (left 1)", n, cardCount(b))
	}
	tapCard(t, b, cbActionApprove, info.ID) // still inside retention
	if got := sender.responded[len(sender.responded)-1]; got != msgAlreadyHandled {
		t.Fatalf("tap inside retention answered %q, want %q", got, msgAlreadyHandled)
	}

	clock.advance(2 * time.Minute)
	if n := b.sweepCards(clock.now()); n != 1 || cardCount(b) != 0 {
		t.Fatalf("sweep past retention evicted %d (left %d), want 1 (left 0)", n, cardCount(b))
	}
	swept := eventsNamed(lines(), "telegram_cards_swept")
	if len(swept) != 1 || swept[0]["evicted"] != float64(1) || swept[0]["remaining"] != float64(0) || swept[0]["trace_id"] != testBootTrace {
		t.Fatalf("telegram_cards_swept lines = %v, want one {evicted:1, remaining:0} with the boot trace_id", swept)
	}
}

// TestUnresolvedCardsEvictedAfterMaxAge (FIX-P-19): a card nobody ever
// tapped is evicted cardMaxAge after it was sent (the engine's own 10-min
// pending TTL expired long before).
func TestUnresolvedCardsEvictedAfterMaxAge(t *testing.T) {
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), &fakeSender{}, nil, newAllowGate(t), fix.Engine, nil)
	clock := newCardClock()
	b.now = clock.now

	sendTestCard(t, b, fix, "never-tapped.md")
	clock.advance(cardMaxAge - time.Second)
	if n := b.sweepCards(clock.now()); n != 0 {
		t.Fatalf("sweep before cardMaxAge evicted %d, want 0", n)
	}
	clock.advance(time.Second)
	if n := b.sweepCards(clock.now()); n != 1 || cardCount(b) != 0 {
		t.Fatalf("sweep at cardMaxAge evicted %d (left %d), want 1 (left 0)", n, cardCount(b))
	}
}

// TestCardsHardCapEvictsOldest (FIX-P-19): beyond maxTrackedCards live
// cards, the oldest-sent ones go first.
func TestCardsHardCapEvictsOldest(t *testing.T) {
	b := newTestBot(t, testConfig(), &fakeSender{}, nil, newAllowGate(t), nil, nil)
	base := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	total := maxTrackedCards + 10
	for i := 0; i < total; i++ {
		b.cards[fmt.Sprintf("card-%04d", i)] = &cardState{sentAt: base.Add(time.Duration(i) * time.Second)}
	}
	now := base.Add(time.Duration(total) * time.Second)
	if n := b.sweepCards(now); n != 10 {
		t.Fatalf("sweep evicted %d, want 10", n)
	}
	if got := cardCount(b); got != maxTrackedCards {
		t.Fatalf("cards left = %d, want %d", got, maxTrackedCards)
	}
	for i := 0; i < total; i++ {
		_, ok := b.cards[fmt.Sprintf("card-%04d", i)]
		if want := i >= 10; ok != want {
			t.Fatalf("card-%04d present=%v, want %v (the 10 oldest evicted)", i, ok, want)
		}
	}
}

// TestTapOnEvictedCardAnswersExpiredSafely (FIX-P-19): a tap on a card
// that was already swept is decided by the ENGINE (here: the pending
// approval expired), answered with the generic expired toast, approves
// nothing and edits nothing - never a panic.
func TestTapOnEvictedCardAnswersExpiredSafely(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	clock := newCardClock()
	b.now = clock.now
	fix.Engine.SetClock(clock.now)

	info := sendTestCard(t, b, fix, "evicted.md")
	clock.advance(cardMaxAge)
	if n := b.sweepCards(clock.now()); n != 1 {
		t.Fatalf("sweep evicted %d, want 1", n)
	}
	tapCard(t, b, cbActionApprove, info.ID)

	if len(sender.responded) != 1 || sender.responded[0] != msgExpired {
		t.Fatalf("responded = %v, want [%q] (the engine refuses the expired id)", sender.responded, msgExpired)
	}
	if len(sender.edited) != 0 {
		t.Fatalf("edited = %+v, want none (no tracked card left to edit)", sender.edited)
	}
	if n := countLedgerRows(t, fix, "policy_feedback_approved"); n != 0 {
		t.Fatalf("policy_feedback_approved rows = %d, want 0", n)
	}
	if cardCount(b) != 0 {
		t.Fatalf("cards = %d after the stale tap, want 0 (markResolved on an unknown id is a no-op)", cardCount(b))
	}
}

// TestOnApprovalInvalidatedSetsResolvedAt (FIX-P-19): a halt-invalidated
// card is edited to the byte-exact halt text, marked resolved at the bot's
// clock, and evicted cardResolvedRetention later.
func TestOnApprovalInvalidatedSetsResolvedAt(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	clock := newCardClock()
	b.now = clock.now

	info := sendTestCard(t, b, fix, "halted.md")
	clock.advance(3 * time.Minute)
	b.OnApprovalInvalidated(context.Background(), info)

	b.mu.Lock()
	card := b.cards[info.ID]
	resolved, resolvedAt := card.resolved, card.resolvedAt
	b.mu.Unlock()
	if !resolved || !resolvedAt.Equal(clock.now()) {
		t.Fatalf("card resolved=%v resolvedAt=%v, want true at %v", resolved, resolvedAt, clock.now())
	}
	if len(sender.edited) != 1 || sender.edited[0].Text != "⛔ Onay geçersiz — görev kullanıcı tarafından durduruldu." {
		t.Fatalf("edited = %+v, want one edit to the byte-exact halt text", sender.edited)
	}
	clock.advance(cardResolvedRetention)
	if n := b.sweepCards(clock.now()); n != 1 {
		t.Fatalf("sweep at resolvedAt+retention evicted %d, want 1", n)
	}
}

// TestSendApprovalCardSweepsOpportunistically (FIX-P-19): every new card
// send sweeps first, so the map stays bounded even with no sweep loop
// running (and in goroutine-free tests).
func TestSendApprovalCardSweepsOpportunistically(t *testing.T) {
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), &fakeSender{}, nil, newAllowGate(t), fix.Engine, nil)
	clock := newCardClock()
	b.now = clock.now

	old := sendTestCard(t, b, fix, "old.md")
	tapCard(t, b, cbActionDeny, old.ID)
	clock.advance(cardMaxAge)
	fresh := sendTestCard(t, b, fix, "fresh.md")

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cards) != 1 || b.cards[fresh.ID] == nil {
		t.Fatalf("cards = %v, want only the fresh card", b.cards)
	}
	if got := b.cards[fresh.ID].sentAt; !got.Equal(clock.now()) {
		t.Fatalf("fresh card sentAt = %v, want the bot clock %v", got, clock.now())
	}
}

// TestSweepCardsLoopEvictsAndStopsWithContext (FIX-P-19): the periodic
// sweep (started by Start) evicts stale cards on its own and returns when
// the daemon's ctx ends.
func TestSweepCardsLoopEvictsAndStopsWithContext(t *testing.T) {
	b := newTestBot(t, testConfig(), &fakeSender{}, nil, newAllowGate(t), nil, nil)
	b.now = time.Now
	b.mu.Lock()
	b.cards["stale"] = &cardState{sentAt: time.Now().Add(-cardMaxAge)}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.sweepCardsLoop(ctx, 5*time.Millisecond)
	}()
	waitFor(t, 2*time.Second, "the loop to evict the stale card", func() bool { return cardCount(b) == 0 })
	cancel()
	waitReturned(t, done, time.Second, "sweepCardsLoop after cancel")
}

// ---- FIX-P-6: chunk cap + no keyboard unless every chunk arrived ----

// TestApprovalCardTooLargeSendsNoticeWithoutKeyboard (FIX-P-6): a card
// that needs more than maxCardChunks messages is not paginated - one
// keyboard-less notice pointing at the local surface, nothing tracked.
func TestApprovalCardTooLargeSendsNoticeWithoutKeyboard(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)

	toolInput := fsWriteInput(t, "buyuk-dosya.txt", fillerLines(2000))
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-too-large", TraceID: "trace-too-large", ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-too-large",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d message(s), want exactly 1 notice", len(sender.sent))
	}
	msg := sender.sent[0]
	if !strings.HasPrefix(msg.Text, "📄 fs_write: ") || !strings.Contains(msg.Text, "'kahya approve "+d.PendingApprovalID+"'") {
		t.Fatalf("notice = %q, want the too-large notice naming the summary and the local command", msg.Text)
	}
	if !strings.Contains(msg.Text, " — onay kartı Telegram için çok büyük (") || !strings.Contains(msg.Text, " parça). Tam diff yalnız yerel yüzeyde: ") {
		t.Fatalf("notice = %q, want the byte-exact Turkish too-large text", msg.Text)
	}
	if msg.Markup != nil {
		t.Fatalf("too-large notice must carry no keyboard, got %+v", msg.Markup)
	}
	if n := cardCount(b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0", n)
	}
	if n := ledger.countKind("telegram_card_too_large"); n != 1 {
		t.Fatalf("telegram_card_too_large rows = %d, want 1", n)
	}
}

// TestApprovalCardPartialSendFailureNoKeyboard (FIX-P-6, WYSIWYE): when a
// MIDDLE chunk fails to reach Telegram while Telegram stays reachable
// (only send #2 fails; every later send would succeed), the keyboard chunk
// is never sent - not even attempted: Telegram gets chunk 1 and then only
// the byte-exact msgCardIncompleteFmt notice, no message carries a
// keyboard, nothing is tracked, and the card is ledgered
// telegram_card_incomplete once for that pending id.
func TestApprovalCardPartialSendFailureNoKeyboard(t *testing.T) {
	sender := &fakeSender{failOn: 2}
	ledger := &fakeLedger{}
	local := &fakeLocalNotifier{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, local)

	toolInput := fsWriteInput(t, "orta-dosya.txt", fillerLines(150))
	chunks := approval.ChunkForTelegram(renderPendingApprovalPayload(b.home, "fs_write", toolInput).Render(), 0)
	if len(chunks) < 3 || len(chunks) > maxCardChunks {
		t.Fatalf("fixture renders %d chunks, want 3..%d so chunk 2 is a middle chunk", len(chunks), maxCardChunks)
	}
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-partial", TraceID: "trace-partial", ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-partial",
	})

	wantNotice := fmt.Sprintf("⚠️ Onay kartı eksik gönderildi; yerel yüzeyden inceleyin: 'kahya approve %s'", d.PendingApprovalID)
	got := sender.allTexts()
	if len(got) != 2 || got[0] != chunks[0] || got[1] != wantNotice {
		t.Fatalf("sent = %q, want exactly [chunk 1, %q]", got, wantNotice)
	}
	assertNoKeyboardAttempted(t, sender, 3)
	if n := cardCount(b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0", n)
	}
	assertCardIncompleteLedgered(t, ledger, d.PendingApprovalID)
	if n := countLocalKind(local, "telegram_send_failed"); n != 1 {
		t.Fatalf("local calls = %q, want exactly one telegram_send_failed line (chunk 2)", local.calls)
	}
}

// TestApprovalCardSendOutageAfterFirstChunkNoKeyboard (FIX-P-6, the
// handbook's failAfter scenario): when Telegram goes away after the first
// chunk (every later send fails, the notice included), only chunk 1 ever
// reached Telegram, the last attempt was the byte-exact incomplete notice
// (the keyboard chunk was never attempted), nothing is tracked, the card
// is ledgered incomplete once, and both failures surface locally.
func TestApprovalCardSendOutageAfterFirstChunkNoKeyboard(t *testing.T) {
	sender := &fakeSender{failAfter: 1}
	ledger := &fakeLedger{}
	local := &fakeLocalNotifier{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, local)

	toolInput := fsWriteInput(t, "orta-dosya.txt", fillerLines(150))
	chunks := approval.ChunkForTelegram(renderPendingApprovalPayload(b.home, "fs_write", toolInput).Render(), 0)
	if len(chunks) < 3 || len(chunks) > maxCardChunks {
		t.Fatalf("fixture renders %d chunks, want 3..%d", len(chunks), maxCardChunks)
	}
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-outage", TraceID: "trace-outage", ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-outage",
	})

	if got := sender.allTexts(); len(got) != 1 || got[0] != chunks[0] {
		t.Fatalf("sent = %q, want exactly chunk 1", got)
	}
	assertNoKeyboardAttempted(t, sender, 3)
	wantNotice := fmt.Sprintf("⚠️ Onay kartı eksik gönderildi; yerel yüzeyden inceleyin: 'kahya approve %s'", d.PendingApprovalID)
	if last := sender.attempted[len(sender.attempted)-1].Text; last != wantNotice {
		t.Fatalf("last send attempt = %q, want the incomplete notice %q", last, wantNotice)
	}
	if n := cardCount(b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0", n)
	}
	assertCardIncompleteLedgered(t, ledger, d.PendingApprovalID)
	if n := countLocalKind(local, "telegram_send_failed"); n != 2 {
		t.Fatalf("local calls = %q, want two telegram_send_failed lines (chunk 2, notice)", local.calls)
	}
}

// assertNoKeyboardAttempted fails unless sender saw exactly wantAttempts
// Send attempts (failed ones included) and none of them carried a keyboard.
func assertNoKeyboardAttempted(t *testing.T, sender *fakeSender, wantAttempts int) {
	t.Helper()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	for i, s := range sender.attempted {
		if s.Markup != nil {
			t.Fatalf("send attempt %d carries the approve keyboard although the card never fully arrived", i+1)
		}
	}
	if len(sender.attempted) != wantAttempts {
		t.Fatalf("send attempts = %d, want %d (chunk 1, the lost chunk 2, the incomplete notice)", len(sender.attempted), wantAttempts)
	}
}

// assertCardIncompleteLedgered fails unless exactly one
// telegram_card_incomplete row exists and it names pendingID.
func assertCardIncompleteLedgered(t *testing.T, ledger *fakeLedger, pendingID string) {
	t.Helper()
	if n := ledger.countKind("telegram_card_incomplete"); n != 1 {
		t.Fatalf("telegram_card_incomplete rows = %d, want 1", n)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for _, e := range ledger.events {
		if e.Kind == "telegram_card_incomplete" && e.Payload["pending_approval_id"] != pendingID {
			t.Fatalf("telegram_card_incomplete payload = %v, want pending_approval_id %s", e.Payload, pendingID)
		}
	}
}

// countLocalKind counts the fakeLocalNotifier lines of one kind.
func countLocalKind(local *fakeLocalNotifier, kind string) int {
	local.mu.Lock()
	defer local.mu.Unlock()
	n := 0
	for _, c := range local.calls {
		if strings.HasPrefix(c, kind+":") {
			n++
		}
	}
	return n
}

// TestApprovalCardWithinCapTrackedOnce (FIX-P-6): a card of exactly
// maxCardChunks messages is still sent in full with the keyboard on the
// last chunk and tracked.
func TestApprovalCardWithinCapTrackedOnce(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)

	// Find a filler size whose rendered card is exactly maxCardChunks
	// chunks (the renderer is deterministic, so this is stable).
	var toolInput []byte
	for n := 300; n < 600; n++ {
		in := fsWriteInput(t, "sinir-dosya.txt", fillerLines(n))
		p := renderPendingApprovalPayload(b.home, "fs_write", in)
		if len(approval.ChunkForTelegram(p.Render(), 0)) == maxCardChunks {
			toolInput = in
			break
		}
	}
	if toolInput == nil {
		t.Fatal("no filler size renders to exactly maxCardChunks chunks")
	}
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-at-cap", TraceID: "trace-at-cap", ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check = %+v, %v", d, err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-at-cap",
	})
	if len(sender.sent) != maxCardChunks {
		t.Fatalf("sent %d message(s), want %d", len(sender.sent), maxCardChunks)
	}
	if sender.sent[len(sender.sent)-1].Markup == nil {
		t.Fatal("final chunk missing its keyboard")
	}
	if n := cardCount(b); n != 1 {
		t.Fatalf("tracked cards = %d, want 1", n)
	}
}
