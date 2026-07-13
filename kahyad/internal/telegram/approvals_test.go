package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

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

// TestW2ApprovalCardMultiChunkKeyboardOnlyOnLast forces a diff large
// enough to require multiple Telegram messages and asserts the keyboard
// is attached to ONLY the final one.
func TestW2ApprovalCardMultiChunkKeyboardOnlyOnLast(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)

	var big strings.Builder
	for i := 0; i < 2000; i++ {
		big.WriteString("line of filler content to force chunking across multiple telegram messages\n")
	}
	toolInput := fsWriteInput(t, "buyuk-dosya.txt", big.String())

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
