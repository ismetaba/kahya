package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/policy"
)

// TestSecretLaneRedactedOnlyTitleSent is the task spec's own acceptance
// criterion, verbatim: "Redaction test green: secret-lane-labeled diff
// never leaves — grep the fake transport's sent messages for any payload
// substring ⇒ zero matches."
func TestSecretLaneRedactedOnlyTitleSent(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)

	home := testHome(t)
	secretDir := filepath.Join(home, "Finans")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretLaneGlobs := []string{filepath.Join(secretDir, "**")}

	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	b.home = home
	b.secretLaneGlobs = secretLaneGlobs

	const path = "~/Finans/hesap-ozeti.txt"
	const secretContent = "IBAN: TR000000000000000000000000 bakiye 1.250.000 TL"
	toolInput := fsWriteInput(t, path, secretContent)

	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-secret", TraceID: "trace-secret", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-secret",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 (redacted title only)", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.Markup != nil {
		t.Fatalf("a secret-lane notice must NEVER carry an approval keyboard, got %+v", msg.Markup)
	}
	want := "🔒 Yerel onay gerekiyor: fs_write (gizli şerit)"
	if msg.Text != want {
		t.Fatalf("sent text = %q, want %q", msg.Text, want)
	}

	// Zero-tolerance: grep EVERY sent message for ANY payload substring -
	// the path, the filename, the directory, and the secret content
	// itself must never appear anywhere.
	forbidden := []string{path, "hesap-ozeti.txt", "Finans", secretContent, "IBAN", "1.250.000"}
	for _, s := range sender.allTexts() {
		for _, bad := range forbidden {
			if strings.Contains(s, bad) {
				t.Errorf("sent message leaked forbidden payload substring %q: %q", bad, s)
			}
		}
	}
}

// TestSecretLaneDeleteAlsoRedacted proves fs_delete gets the identical
// redaction treatment as fs_write (both carry a bare {"path": ...}
// envelope).
func TestSecretLaneDeleteAlsoRedacted(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)

	home := testHome(t)
	secretDir := filepath.Join(home, "Saglik")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	b.home = home
	b.secretLaneGlobs = []string{filepath.Join(secretDir, "**")}

	const path = "~/Saglik/tahlil-sonucu.pdf"
	toolInput := fsWriteInput(t, path, "")

	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_delete", TaskID: "task-secret2", TraceID: "trace-secret2", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_delete", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-secret2",
	})

	if len(sender.sent) != 1 || sender.sent[0].Text != "🔒 Yerel onay gerekiyor: fs_delete (gizli şerit)" {
		t.Fatalf("sent = %+v, want a single redacted fs_delete notice", sender.sent)
	}
	for _, s := range sender.allTexts() {
		if strings.Contains(s, "Saglik") || strings.Contains(s, "tahlil-sonucu") {
			t.Errorf("sent message leaked the secret-lane path: %q", s)
		}
	}
}

// TestSecretLaneW3StillRedacted is the BLOCKER regression test: a
// secret-lane-labeled fs_write classified W3 (a valid policy.yaml config -
// e.g. an fs_write rule with class: W3, reversible: false) must send ONLY
// the redacted title to Telegram, exactly like W1/W2 - never the W3
// notify-only text (msgW3WaitFmt), which would otherwise embed the REAL
// path via renderPendingApprovalPayload's Summary ("fs_write: ~/Kimlik/
// tc-kimlik-no.txt ..."). Before the fix, OnPendingApproval only ran the
// isSecretLane check inside the W1/W2 branch, so this exact scenario leaked
// the secret path straight to Telegram.
func TestSecretLaneW3StillRedacted(t *testing.T) {
	sender := &fakeSender{}

	home := testHome(t)
	secretDir := filepath.Join(home, "Kimlik")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.home = home
	b.secretLaneGlobs = []string{filepath.Join(secretDir, "**")}

	const path = "~/Kimlik/tc-kimlik-no.txt"
	const secretContent = "12345678901"
	toolInput := fsWriteInput(t, path, secretContent)

	// No real policy.Engine mint is needed here: OnPendingApproval never
	// consults the engine itself (only handleCallback does), so a
	// fabricated pending id is enough to exercise the notify path being
	// tested - the whole point is that Class: policy.ClassW3 must NOT
	// change what gets redacted.
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: "test-pending-w3-secret", Tool: "fs_write", Class: policy.ClassW3,
		ToolInput: toolInput, TraceID: "trace-secret-w3",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 (redacted title only, even for W3)", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.Markup != nil {
		t.Fatalf("a secret-lane W3 notice must never carry a keyboard, got %+v", msg.Markup)
	}
	want := "🔒 Yerel onay gerekiyor: fs_write (gizli şerit)"
	if msg.Text != want {
		t.Fatalf("sent text = %q, want %q (must NOT be the W3 wait-notice format)", msg.Text, want)
	}

	// Zero-tolerance: grep EVERY sent message for ANY component of the
	// secret path (directory, filename, full path) and the secret content
	// itself - none of it may appear anywhere, and the W3-specific
	// "waiting for approval" wording/id must not leak either.
	forbidden := []string{path, "tc-kimlik-no", "Kimlik", secretContent, "(W3)", "test-pending-w3-secret"}
	for _, s := range sender.allTexts() {
		for _, bad := range forbidden {
			if strings.Contains(s, bad) {
				t.Errorf("sent message leaked forbidden payload substring %q: %q", bad, s)
			}
		}
	}
}

// TestSecretLaneW3DeleteAlsoRedacted is TestSecretLaneW3StillRedacted's
// fs_delete counterpart (task spec: "Add the same for a secret-lane W3
// fs_delete").
func TestSecretLaneW3DeleteAlsoRedacted(t *testing.T) {
	sender := &fakeSender{}

	home := testHome(t)
	secretDir := filepath.Join(home, "Kimlik")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.home = home
	b.secretLaneGlobs = []string{filepath.Join(secretDir, "**")}

	const path = "~/Kimlik/tc-kimlik-no.txt"
	toolInput := fsWriteInput(t, path, "")

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: "test-pending-w3-secret-delete", Tool: "fs_delete", Class: policy.ClassW3,
		ToolInput: toolInput, TraceID: "trace-secret-w3-delete",
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 (redacted title only, even for W3)", len(sender.sent))
	}
	want := "🔒 Yerel onay gerekiyor: fs_delete (gizli şerit)"
	if sender.sent[0].Text != want {
		t.Fatalf("sent text = %q, want %q", sender.sent[0].Text, want)
	}
	if sender.sent[0].Markup != nil {
		t.Fatalf("a secret-lane W3 delete notice must never carry a keyboard, got %+v", sender.sent[0].Markup)
	}

	forbidden := []string{path, "tc-kimlik-no", "Kimlik", "(W3)", "test-pending-w3-secret-delete"}
	for _, s := range sender.allTexts() {
		for _, bad := range forbidden {
			if strings.Contains(s, bad) {
				t.Errorf("sent message leaked forbidden payload substring %q: %q", bad, s)
			}
		}
	}
}

// TestNonSecretLanePathSendsFullDiff is the negative control: a path
// OUTSIDE secretLaneGlobs gets the ordinary full byte-exact diff + keyboard
// (proves the redact check is not over-firing on every payload).
func TestNonSecretLanePathSendsFullDiff(t *testing.T) {
	sender := &fakeSender{}
	fix := newPolicyFixture(t)

	home := testHome(t)
	if err := os.MkdirAll(filepath.Join(home, "Finans"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), fix.Engine, nil)
	b.home = home
	b.secretLaneGlobs = []string{filepath.Join(home, "Finans", "**")}

	toolInput := fsWriteInput(t, "~/Belgeler/gunluk-not.txt", "bugün hava güzel")
	d, err := fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: "fs_write", TaskID: "task-ok", TraceID: "trace-ok", ToolInput: toolInput,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: d.PendingApprovalID, Tool: "fs_write", Class: policy.ClassW2, ToolInput: toolInput, TraceID: "trace-ok",
	})

	if len(sender.sent) == 0 {
		t.Fatal("no message sent")
	}
	full := strings.Join(sender.allTexts(), "")
	if !strings.Contains(full, "bugün hava güzel") {
		t.Errorf("non-secret-lane path must still send the real diff; got:\n%s", full)
	}
	if sender.sent[len(sender.sent)-1].Markup == nil {
		t.Error("non-secret-lane W2 card must still carry the approval keyboard")
	}
}

// TestIsSecretLaneDoesNotFlagOrdinaryWordContainingEFT is review finding
// B-2 at the Telegram redaction seam: fs_write content that merely
// contains "eft" inside an ordinary word ("defter") is not secret-lane,
// while the whole word "EFT" still is.
func TestIsSecretLaneDoesNotFlagOrdinaryWordContainingEFT(t *testing.T) {
	home := testHome(t)
	if isSecretLane(home, nil, "fs_write", fsWriteInput(t, "~/notlar/defter.md", "Bu defter çok güzel")) {
		t.Error(`isSecretLane("Bu defter çok güzel") = true, want false (no word "eft")`)
	}
	if !isSecretLane(home, nil, "fs_write", fsWriteInput(t, "~/notlar/defter.md", "EFT ile 500 TL gönder")) {
		t.Error(`isSecretLane("EFT ile 500 TL gönder") = false, want true (whole word "EFT")`)
	}
}

// ---- FIX-P-6: the second gate classifies the EXACT outbound text ----

// redactFixture is the FIX-P-6 redaction-test bot: a real engine (real
// pending ids), a fakeLedger, a resolved temp home with ~/Finans/** as the
// only secret-lane glob.
type redactFixture struct {
	b      *Bot
	sender *fakeSender
	ledger *fakeLedger
	fix    policyFixture
	home   string
}

func newRedactFixture(t *testing.T) redactFixture {
	t.Helper()
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	fix := newPolicyFixture(t)
	home := testHome(t)
	if err := os.MkdirAll(filepath.Join(home, "Finans"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), fix.Engine, nil)
	b.home = home
	b.secretLaneGlobs = []string{filepath.Join(home, "Finans", "**")}
	return redactFixture{b: b, sender: sender, ledger: ledger, fix: fix, home: home}
}

// writeHomeFile writes content at home/rel (parents created).
func (r redactFixture) writeHomeFile(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(r.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deliver mints a real pending approval for (tool, toolInput) and hands it
// to OnPendingApproval as class.
func (r redactFixture) deliver(t *testing.T, tool string, class policy.ActionClass, toolInput []byte, traceID string) policy.PendingApprovalInfo {
	t.Helper()
	d, err := r.fix.Engine.Check(context.Background(), policy.CheckInput{
		Tool: tool, TaskID: "task-" + traceID, TraceID: traceID, ToolInput: toolInput,
	})
	if err != nil || d.Result != policy.ResultNeedsApproval {
		t.Fatalf("Check(%s) = %+v, %v; want NEEDS_APPROVAL", tool, d, err)
	}
	info := policy.PendingApprovalInfo{ID: d.PendingApprovalID, Tool: tool, Class: class, ToolInput: toolInput, TraceID: traceID}
	r.b.OnPendingApproval(context.Background(), info)
	return info
}

// redactedReasons returns every telegram_card_redacted row's reason.
func redactedReasons(l *fakeLedger) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if e.Kind == "telegram_card_redacted" {
			reason, _ := e.Payload["reason"].(string)
			out = append(out, reason)
		}
	}
	return out
}

// assertOnlyRedactedTitle: exactly one message, the byte-exact redacted
// title for tool, no keyboard, no card tracked, none of forbidden in any
// sent text, and one telegram_card_redacted row whose reason starts with
// reasonPrefix.
func assertOnlyRedactedTitle(t *testing.T, r redactFixture, tool string, forbidden []string, reasonPrefix string) {
	t.Helper()
	sent := r.sender.sent
	if len(sent) != 1 {
		t.Fatalf("sent count = %d (%q), want exactly 1 (redacted title only)", len(sent), r.sender.allTexts())
	}
	if want := "🔒 Yerel onay gerekiyor: " + tool + " (gizli şerit)"; sent[0].Text != want {
		t.Fatalf("sent text = %q, want %q", sent[0].Text, want)
	}
	if sent[0].Markup != nil {
		t.Fatalf("a redacted notice must never carry a keyboard, got %+v", sent[0].Markup)
	}
	for _, s := range r.sender.allTexts() {
		for _, bad := range forbidden {
			if strings.Contains(s, bad) {
				t.Errorf("sent message leaked %q: %q", bad, s)
			}
		}
	}
	if n := cardCount(r.b); n != 0 {
		t.Fatalf("tracked cards = %d, want 0 (no card was sent)", n)
	}
	reasons := redactedReasons(r.ledger)
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], reasonPrefix) {
		t.Fatalf("telegram_card_redacted reasons = %q, want exactly one starting with %q", reasons, reasonPrefix)
	}
}

const testIBAN = "TR33 0006 1005 1978 6457 8413 26"

// TestFsDeleteOldContentWithIBANRedacted (FIX-P-6): fs_delete carries no
// content, but its card lists the OLD file content read from disk - an
// IBAN there must never reach Telegram, even though the path is outside
// every secret-lane glob.
func TestFsDeleteOldContentWithIBANRedacted(t *testing.T) {
	r := newRedactFixture(t)
	r.writeHomeFile(t, "Belgeler/hesap.txt", "- süt al\n- IBAN: "+testIBAN+"\n")

	r.deliver(t, "fs_delete", policy.ClassW1, []byte(`{"path":"~/Belgeler/hesap.txt"}`), "trace-p6-delete-iban")

	assertOnlyRedactedTitle(t, r, "fs_delete", []string{"TR33", "hesap.txt", "süt", "Belgeler"}, "content:iban")
}

// TestFsWriteRemovingIBANLineRedactedViaOldContent (FIX-P-6): the NEW
// content is clean (the first gate passes), but the diff's removed line
// carries the old IBAN.
func TestFsWriteRemovingIBANLineRedactedViaOldContent(t *testing.T) {
	r := newRedactFixture(t)
	r.writeHomeFile(t, "Belgeler/hesap.txt", "- süt al\n- IBAN: "+testIBAN+"\n")

	r.deliver(t, "fs_write", policy.ClassW2, fsWriteInput(t, "~/Belgeler/hesap.txt", "- süt al\n"), "trace-p6-write-iban")

	assertOnlyRedactedTitle(t, r, "fs_write", []string{"TR33", "hesap.txt", "süt"}, "content:iban")
}

// TestApplescriptBodyWithTCKNRedacted (FIX-P-6): a non-fs tool's body is
// rendered as a raw listing of tool_input; a TCKN inside the script never
// reaches Telegram.
func TestApplescriptBodyWithTCKNRedacted(t *testing.T) {
	r := newRedactFixture(t)
	input := []byte(`{"script":"display dialog \"TC 10000000146\"","target_app":"Finder"}`)

	r.deliver(t, "applescript_run", policy.ClassW2, input, "trace-p6-applescript-tckn")

	assertOnlyRedactedTitle(t, r, "applescript_run", []string{"10000000146", "display dialog", "Finder"}, "content:tckn")
}

// TestShellDockerScriptWithCardNumberRedacted (FIX-P-6): shell_docker's
// tool_input is the RAW script bytes (mcp/shell/runner.go), not a JSON
// envelope; a card number in it never reaches Telegram.
func TestShellDockerScriptWithCardNumberRedacted(t *testing.T) {
	r := newRedactFixture(t)

	r.deliver(t, "shell_docker", policy.ClassW2, []byte("echo 4111 1111 1111 1111\n"), "trace-p6-shell-card")

	assertOnlyRedactedTitle(t, r, "shell_docker", []string{"4111", "echo"}, "content:card_number")
}

// TestW3NoticeSummaryWithIBANRedacted (FIX-P-6): a W3 notice embeds the
// payload summary (the path); an IBAN-shaped path outside every glob is
// redacted, and the W3 wait text is never sent.
func TestW3NoticeSummaryWithIBANRedacted(t *testing.T) {
	r := newRedactFixture(t)
	input := fsWriteInput(t, "~/Belgeler/"+testIBAN+".txt", "merhaba")

	r.b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: "test-pending-w3-iban-path", Tool: "fs_write", Class: policy.ClassW3, ToolInput: input, TraceID: "trace-p6-w3-iban",
	})

	assertOnlyRedactedTitle(t, r, "fs_write", []string{"TR33", "⏳ Yerelde onay bekleniyor", "test-pending-w3-iban-path", "Belgeler"}, "content:iban")
}

// TestOutboundZeroWidthHiddenIBANRedacted (FIX-P-6): a zero-width space
// hidden inside an IBAN renders as a visible "<U+200B>" escape that the
// raw regexes miss; the escape-stripped canonical pass still catches it.
func TestOutboundZeroWidthHiddenIBANRedacted(t *testing.T) {
	r := newRedactFixture(t)
	r.writeHomeFile(t, "Belgeler/not.txt", "Not: TR33\u200b0006 1005 1978 6457 8413 26\n")

	r.deliver(t, "fs_delete", policy.ClassW2, []byte(`{"path":"~/Belgeler/not.txt"}`), "trace-p6-zw-iban")

	assertOnlyRedactedTitle(t, r, "fs_delete", []string{"TR33", "8413", "not.txt"}, "canonical:iban")
}

// TestSecretLaneMalformedGlobFailsClosed (FIX-P-6a): a malformed
// secret_lane_globs entry makes the redaction decision unknown, so the
// card is redacted instead of sent in full.
func TestSecretLaneMalformedGlobFailsClosed(t *testing.T) {
	r := newRedactFixture(t)
	r.b.secretLaneGlobs = []string{"["}

	r.deliver(t, "fs_write", policy.ClassW2, fsWriteInput(t, "~/Belgeler/x.txt", "merhaba"), "trace-p6a-bad-glob")

	assertOnlyRedactedTitle(t, r, "fs_write", []string{"x.txt", "merhaba"}, "glob_error")
}

// TestSecretLaneUncanonicalizablePathFailsClosed (FIX-P-6a): a path
// mcp/fs cannot canonicalize (here: a zero-width rune) cannot be cleared
// against the globs, so the card is redacted.
func TestSecretLaneUncanonicalizablePathFailsClosed(t *testing.T) {
	r := newRedactFixture(t)
	input := fsWriteInput(t, "~/Finans\u200b/plan.txt", "merhaba")

	r.deliver(t, "fs_write", policy.ClassW2, input, "trace-p6a-uncanonical")

	assertOnlyRedactedTitle(t, r, "fs_write", []string{"plan.txt", "Finans", "merhaba"}, "canonicalize_error")
}
