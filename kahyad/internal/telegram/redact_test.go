package telegram

import (
	"context"
	"encoding/json"
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

// TestFsCardAPFSFoldedSecretGlobPathRedacted (FIX-FINAL attacker2-apfs-
// fold-glob-bypass): an fs_write / fs_delete card whose path is the long-s
// or ligature spelling APFS opens as a secret-glob file is redacted like
// the plain spelling.
func TestFsCardAPFSFoldedSecretGlobPathRedacted(t *testing.T) {
	for trace, path := range map[string]string{
		"trace-fold-long-s": "~/Finan\u017f/plan.txt",
		"trace-fold-nested": "~/FINAN\u017f/a/plan.txt",
	} {
		r := newRedactFixture(t)
		r.deliver(t, "fs_write", policy.ClassW2, fsWriteInput(t, path, "market listesi"), trace)
		assertOnlyRedactedTitle(t, r, "fs_write", []string{"plan.txt", "market"}, redactReasonGlob)
	}
	r := newRedactFixture(t)
	r.deliver(t, "fs_delete", policy.ClassW1, []byte(`{"path":"~/Finan\u017f/plan.txt"}`), "trace-fold-del")
	assertOnlyRedactedTitle(t, r, "fs_delete", []string{"plan.txt"}, redactReasonGlob)
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

// scriptEnvelope builds an applescript_run/jxa_run tool_input exactly the
// way mcp/osascript/runner.go's buildScriptToolInput does (json.Marshal of
// {script, target_app}): a newline or tab inside the script travels as the
// two bytes `\n` / `\t`, which is what the raw card listing shows.
func scriptEnvelope(t *testing.T, script, targetApp string) []byte {
	t.Helper()
	b, err := json.Marshal(struct {
		Script    string `json:"script"`
		TargetApp string `json:"target_app,omitempty"`
	}{script, targetApp})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestApplescriptMultilineBodyIBANRedacted (FIX-P-6 review): an ordinary
// multi-line script whose IBAN starts a new line. The raw listing shows
// the JSON escape `\nTR33 ...`, which has no word boundary before "TR33";
// the escape-decoded pass of outboundSecretLane still catches it.
func TestApplescriptMultilineBodyIBANRedacted(t *testing.T) {
	r := newRedactFixture(t)
	input := scriptEnvelope(t, "set notlar to \"kira\"\n"+testIBAN+"\ndisplay dialog notlar", "Finder")

	r.deliver(t, "applescript_run", policy.ClassW2, input, "trace-p6-applescript-multiline")

	assertOnlyRedactedTitle(t, r, "applescript_run", []string{"TR33", "8413", "display dialog", "Finder"}, "escape:iban")
}

// TestMemoryWriteMultilineNoteIBANRedacted (FIX-P-6 review): memory_write
// is W1 reversible, so its card keeps the approve keyboard; a markdown
// note whose IBAN starts a new line (JSON `\n\nTR33 ...`) is redacted -
// one title, no keyboard, one telegram_card_redacted row.
func TestMemoryWriteMultilineNoteIBANRedacted(t *testing.T) {
	r := newRedactFixture(t)
	input, err := json.Marshal(map[string]string{
		"file":    "inbox/2026-10-15.md",
		"content": "# Notlar\n\n" + testIBAN + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	r.deliver(t, "memory_write", policy.ClassW1, input, "trace-p6-memwrite-multiline")

	assertOnlyRedactedTitle(t, r, "memory_write", []string{"TR33", "8413", "Notlar", "inbox"}, "escape:iban")
}

// TestEscapedToolBodySecretsRedacted (FIX-P-6 review): every escape form
// a raw card listing can carry a secret behind - a JSON control escape
// before a TCKN or card number, a \uXXXX-escaped letter or separator, a
// JSON document nested inside a JSON tool_input (double escape), a
// shell_host argument list, and literal escapes inside OLD file content
// on disk - is decoded and classified before anything is sent.
func TestEscapedToolBodySecretsRedacted(t *testing.T) {
	cases := []struct {
		name, tool string
		class      policy.ActionClass
		input      func(t *testing.T, r redactFixture) []byte
		forbidden  []string
		reason     string
	}{
		{
			name: "jxa_tckn_after_newline", tool: "jxa_run", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				return scriptEnvelope(t, "const x = 1;\n10000000146", "")
			},
			forbidden: []string{"10000000146", "const x"}, reason: "escape:tckn",
		},
		{
			name: "applescript_card_after_tab", tool: "applescript_run", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				return scriptEnvelope(t, "set x to \"kart:\t4111 1111 1111 1111\"", "Finder")
			},
			forbidden: []string{"4111", "kart"}, reason: "escape:card_number",
		},
		{
			name: "unicode_escaped_letter", tool: "applescript_run", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				return []byte(`{"script":"set x to \"\u0054R33 0006 1005 1978 6457 8413 26\""}`)
			},
			forbidden: []string{"R33 0006", "8413"}, reason: "escape:iban",
		},
		{
			name: "unicode_escaped_separator", tool: "applescript_run", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				return []byte(`{"script":"x\u0020TR33 0006 1005 1978 6457 8413 26"}`)
			},
			forbidden: []string{"TR33", "8413"}, reason: "escape:iban",
		},
		{
			name: "nested_json_double_escape", tool: "memory_write", class: policy.ClassW1,
			input: func(t *testing.T, r redactFixture) []byte {
				inner, err := json.Marshal(map[string]string{"not": "kira\n" + testIBAN})
				if err != nil {
					t.Fatal(err)
				}
				outer, err := json.Marshal(map[string]string{"file": "inbox/ayar.md", "content": string(inner)})
				if err != nil {
					t.Fatal(err)
				}
				return outer
			},
			forbidden: []string{"TR33", "8413", "kira"}, reason: "escape:iban",
		},
		{
			name: "shell_host_arg_newline", tool: "shell_host", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				b, err := json.Marshal(struct {
					Command  string   `json:"command"`
					RepoPath string   `json:"repo_path,omitempty"`
					Args     []string `json:"args,omitempty"`
				}{"git", "/tmp/repo", []string{"commit", "-m", "kira\n" + testIBAN}})
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			forbidden: []string{"TR33", "8413", "commit"}, reason: "escape:iban",
		},
		{
			name: "old_file_content_literal_escape", tool: "fs_delete", class: policy.ClassW1,
			input: func(t *testing.T, r redactFixture) []byte {
				r.writeHomeFile(t, "Belgeler/ayar.json", `{"not": "kira\n`+testIBAN+`"}`+"\n")
				return []byte(`{"path":"~/Belgeler/ayar.json"}`)
			},
			forbidden: []string{"TR33", "8413", "ayar.json", "kira"}, reason: "escape:iban",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRedactFixture(t)
			r.deliver(t, tc.tool, tc.class, tc.input(t, r), "trace-p6-escape-"+tc.name)
			assertOnlyRedactedTitle(t, r, tc.tool, tc.forbidden, tc.reason)
		})
	}
}

// TestEscapedCleanBodyStillSendsFullCard (FIX-P-6 review, false-positive
// guard): decoding escapes never redacts a clean card - a JSON file whose
// content carries ordinary `\n` / `\u00e7` escapes, and a clean multi-line
// memory_write note, still get the full card with the keyboard.
func TestEscapedCleanBodyStillSendsFullCard(t *testing.T) {
	cases := []struct {
		name, tool string
		class      policy.ActionClass
		input      func(t *testing.T, r redactFixture) []byte
		want       string
	}{
		{
			name: "fs_write_json_with_escapes", tool: "fs_write", class: policy.ClassW2,
			input: func(t *testing.T, r redactFixture) []byte {
				return fsWriteInput(t, "~/Belgeler/ayar.json", `{"not": "satir 1\nsatir 2 \u00e7ay", "yol": "C:\\temp"}`+"\n")
			},
			want: `satir 1\nsatir 2 \u00e7ay`,
		},
		{
			name: "memory_write_multiline_note", tool: "memory_write", class: policy.ClassW1,
			input: func(t *testing.T, r redactFixture) []byte {
				b, err := json.Marshal(map[string]string{"file": "inbox/2026-10-15.md", "content": "# Notlar\n\nbugün hava güzel\n\tsüt al\n"})
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			want: `bugün hava güzel\n\tsüt al`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRedactFixture(t)
			r.deliver(t, tc.tool, tc.class, tc.input(t, r), "trace-p6-escape-clean-"+tc.name)
			sent := r.sender.sent
			if len(sent) != 1 {
				t.Fatalf("sent = %q, want exactly the one-chunk card", r.sender.allTexts())
			}
			if !strings.Contains(sent[0].Text, tc.want) {
				t.Fatalf("card %q does not carry %q", sent[0].Text, tc.want)
			}
			if sent[0].Markup == nil {
				t.Fatalf("a clean reversible card must carry the approve keyboard")
			}
			if reasons := redactedReasons(r.ledger); len(reasons) != 0 {
				t.Fatalf("telegram_card_redacted reasons = %q, want none", reasons)
			}
		})
	}
}

// TestFoldBackslashEscapesDecodesOneLevel pins the decoder behind
// outboundSecretLane's escape pass: one nesting level per call, JSON
// escapes plus \xHH (ASCII) and \u{...}, surrogate pairs joined, and every
// malformed or unknown escape kept verbatim (so a clean round is a no-op).
func TestFoldBackslashEscapesDecodesOneLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		{`satir\nTR33\tx\r\b\f`, "satir\nTR33\tx\r\b\f"},
		{`\"q\" \\ \/ \'`, `"q" \ / '`},
		{`TR33 ç \u{1F600} 😀`, "TR33 ç \U0001F600 \U0001F600"},
		{`\uD83D tek`, "� tek"},
		{`\x54R33 \xC4 \x4`, `TR33 \xC4 \x4`},
		{`a\\nb`, `a\nb`},
		{`\q \u12 \u{} \u{110000} sonda\`, `\q \u12 \u{} \u{110000} sonda\`},
		{"temiz metin", "temiz metin"},
	}
	for _, tc := range cases {
		if got := foldBackslashEscapes(tc.in); got != tc.want {
			t.Errorf("foldBackslashEscapes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
