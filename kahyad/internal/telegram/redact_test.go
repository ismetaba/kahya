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
