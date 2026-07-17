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

// nonFSToolInput marshals the raw tool_input envelope a NON-fs tool
// (applescript_run/shell_docker/shell_host/...) carries. render.go's
// renderPendingApprovalPayload dumps this JSON VERBATIM into the Telegram
// approval card (via approval.BuildOsascript), which is exactly why
// isSecretLane's default branch must scan string(toolInput). review-fix #5.
func nonFSToolInput(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal tool_input: %v", err)
	}
	return b
}

// assertNonFSToolRedacted drives OnPendingApproval (Class=W2 - every one of
// these tools is W2 in policy.yaml) for a non-fs tool whose raw tool_input
// embeds secret-lane content, and asserts Telegram received ONLY the
// redacted notice: no keyboard, and none of the script/command/secret bytes.
// review-fix #5: without the default-branch content scan, a W2 card renders
// the ENTIRE tool_input JSON verbatim (render.go BuildOsascript), so the
// secret would be shipped straight to api.telegram.org.
func assertNonFSToolRedacted(t *testing.T, tool string, toolInput []byte, forbidden []string) {
	t.Helper()
	sender := &fakeSender{}
	// secretLaneGlobs is deliberately empty: a non-fs tool never reaches the
	// glob branch, so it is the CONTENT scan (secretlane.ClassifyDeterministic
	// over the raw tool_input JSON) - not any path glob - that must fire here.
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.home = testHome(t)
	b.secretLaneGlobs = nil

	if !isSecretLane(b.home, b.secretLaneGlobs, tool, toolInput) {
		t.Fatalf("isSecretLane(%s) = false, want true (secret-lane content in raw tool_input)", tool)
	}

	// No real policy.Engine mint is needed: OnPendingApproval redacts BEFORE
	// consulting the engine (only handleCallback ever does), so a fabricated
	// pending id is enough to exercise the notify path being tested.
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: "test-pending-w2-secret", Tool: tool, Class: policy.ClassW2,
		ToolInput: toolInput, TraceID: "trace-secret-" + tool,
	})

	if len(sender.sent) != 1 {
		t.Fatalf("sent count = %d, want exactly 1 (redacted title only)", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.Markup != nil {
		t.Fatalf("a secret-lane notice must NEVER carry an approval keyboard, got %+v", msg.Markup)
	}
	if want := redactedNoticeText(tool); msg.Text != want {
		t.Fatalf("sent text = %q, want %q (must be the redacted notice, not the tool_input)", msg.Text, want)
	}

	// Zero-tolerance: grep EVERY sent message for the secret value and its
	// surrounding script/command bytes - none may appear anywhere.
	for _, s := range sender.allTexts() {
		for _, bad := range forbidden {
			if strings.Contains(s, bad) {
				t.Errorf("sent message leaked forbidden secret substring %q: %q", bad, s)
			}
		}
	}
}

// TestSecretLaneAppleScriptRedacted: an applescript_run (W2) whose
// {"script":...} embeds a valid-checksum TCKN must send ONLY the redacted
// notice to Telegram, never the script bytes. review-fix #5.
func TestSecretLaneAppleScriptRedacted(t *testing.T) {
	// 10000000146 is the valid-checksum TCKN fixture from
	// kahyad/internal/secretlane/classifier_test.go. No "TC kimlik" keyword
	// cue is present, so the checksum-verified TCKN itself is what trips the
	// deterministic pre-pass.
	const tckn = "10000000146"
	toolInput := nonFSToolInput(t, map[string]any{
		"script": `set kayit to "musteri no " & "` + tckn + `"`,
	})
	assertNonFSToolRedacted(t, "applescript_run", toolInput, []string{tckn, "set kayit to", "musteri no"})
}

// TestSecretLaneShellDockerRedacted: a shell_docker (W2) whose command
// echoes a valid Turkish IBAN must be redacted. review-fix #5.
func TestSecretLaneShellDockerRedacted(t *testing.T) {
	// TR330006100519786457841326 is the unspaced IBAN fixture from
	// kahyad/internal/secretlane/classifier_test.go.
	const iban = "TR330006100519786457841326"
	toolInput := nonFSToolInput(t, map[string]any{
		"command": "echo " + iban + " > /tmp/hesap.txt",
	})
	assertNonFSToolRedacted(t, "shell_docker", toolInput, []string{iban, "echo", "/tmp/hesap.txt"})
}

// TestSecretLaneShellHostRedacted: a shell_host (W2) with command+args
// carrying a Luhn-valid card number must be redacted. review-fix #5.
func TestSecretLaneShellHostRedacted(t *testing.T) {
	// 4539148803436467 is the Luhn-valid test Visa PAN fixture (unspaced)
	// from kahyad/internal/secretlane/classifier_test.go.
	const card = "4539148803436467"
	toolInput := nonFSToolInput(t, map[string]any{
		"command": "curl",
		"args":    []string{"--data", "pan=" + card, "https://example.test/pay"},
	})
	assertNonFSToolRedacted(t, "shell_host", toolInput, []string{card, "curl", "--data"})
}

// TestNonSecretLaneShellDockerSendsFullCard is the negative control: a
// shell_docker (W2) whose tool_input carries NO secret-lane content must
// still get the ordinary full inline-keyboard card (proves the default-
// branch content scan does not over-fire on every non-fs tool). review-fix
// #5 counterpart to TestNonSecretLanePathSendsFullDiff.
func TestNonSecretLaneShellDockerSendsFullCard(t *testing.T) {
	sender := &fakeSender{}
	// engine is nil (unused: sendApprovalCard never consults it) but the
	// pending id MUST be valid hex so approvalMarkup can build the keyboard.
	b := newTestBot(t, testConfig(), sender, nil, newAllowGate(t), nil, nil)
	b.secretLaneGlobs = nil

	const command = "echo merhaba dunya"
	toolInput := nonFSToolInput(t, map[string]any{"command": command})

	if isSecretLane(b.home, b.secretLaneGlobs, "shell_docker", toolInput) {
		t.Fatal("isSecretLane(shell_docker) = true for a benign command, want false")
	}

	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: strings.Repeat("ab", 32), Tool: "shell_docker", Class: policy.ClassW2,
		ToolInput: toolInput, TraceID: "trace-ok-docker",
	})

	if len(sender.sent) == 0 {
		t.Fatal("no message sent")
	}
	full := strings.Join(sender.allTexts(), "")
	if !strings.Contains(full, command) {
		t.Errorf("non-secret-lane shell_docker must still send the full tool_input card; got:\n%s", full)
	}
	if sender.sent[len(sender.sent)-1].Markup == nil {
		t.Error("non-secret-lane W2 card must still carry the approval keyboard")
	}
}
