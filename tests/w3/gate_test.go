// Package w3gate implements the five permanent W3-10 acceptance-gate
// tests: one per clause of HANDOFF §6's W3 acceptance sentence (see the
// task file's own clause -> test mapping table,
// tasks/w3-policy-tools/W3-10-w3-acceptance.md). Every test here drives a
// REAL, separately-built bin/kahyad child process (harness_test.go) plus,
// where the gate names a remote surface, a FAKE transport standing in for
// it: tests/w3/faketelegram for the Telegram Bot API, tests/e2e/
// mockanthropic for the Anthropic API - neither a live BotFather token nor
// a cloud credential is ever required to run this file.
//
// KAHYA_DOCKER_TESTS=1 go test ./tests/w3/... -run TestGate -v runs all
// five with zero skips when Docker is up (Gate 4 is the only one that
// needs it; requireDockerTests SKIPS, not fails, when the var is unset -
// `make test` exports it automatically whenever `docker info` succeeds).
package w3gate

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/tests/e2e/mockanthropic"
	"kahya/tests/w3/faketelegram"
)

const (
	gateTelegramChatID = int64(910001)
	gateTelegramUserID = int64(920002)
)

// encodeCallbackData replicates kahyad/internal/telegram/approvals.go's
// unexported encodeCallbackData (that package is under kahyad/internal/ -
// not importable here, this file's own package doc comment) purely as a
// STABLE WIRE FORMAT: one action byte ('A'pprove/'D'eny) followed by the
// pending_approval_id's 32 raw bytes, base64url-encoded (no padding). This
// is exactly what a real inline-button tap's callback_data contains -
// reconstructing it here is what lets faketelegram.PushCallback simulate
// one authentically, whether "legitimate" (Gate 1) or "forged" (Gate 2 -
// W3 never sends a button at all, so ANY callback_data for a W3 pending id
// is, by definition, not from a real button).
func encodeCallbackData(t *testing.T, action byte, pendingApprovalID string) string {
	t.Helper()
	raw, err := hex.DecodeString(pendingApprovalID)
	if err != nil {
		t.Fatalf("encodeCallbackData: pending_approval_id %q is not hex: %v", pendingApprovalID, err)
	}
	return string(action) + base64.RawURLEncoding.EncodeToString(raw)
}

// waitForSentMessages polls fake.SentMessages() until at least want
// messages have arrived (Telegram delivery is asynchronous - main.go fires
// it via "go tgBot.OnPendingApproval(...)" precisely so a slow send can
// never delay a policy decision) or the timeout elapses.
func waitForSentMessages(t *testing.T, fake *faketelegram.Server, want int, timeout time.Duration) []faketelegram.SentMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sent := fake.SentMessages()
		if len(sent) >= want {
			return sent
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake telegram transport got %d message(s) within %v, want >= %d: %+v", len(sent), timeout, want, sent)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ============================================================
// Gate 1 — W2 action requires byte-exact Telegram diff approval
// ============================================================

// TestGate1_W2RequiresByteExactTelegramDiffApproval is the §6 W3
// acceptance sentence's first clause: "W2 bir eylem Telegram'dan byte-tam
// diff ile onay istiyor". A fresh (L0) fs_write is denied pending
// approval; the fake Telegram transport receives EXACTLY one card whose
// text is byte-identical (not merely substring-matching) to the SAME
// pending approval's own GET /policy/approvals?id=<id> rendering — an
// independent cross-check between kahyad/internal/telegram/render.go's
// and kahyad/internal/server/approvals.go's own (deliberately duplicated,
// per each file's doc comment) renderers, both of which build on
// kahyad/internal/approval.BuildFileEdit — carrying an inline Onayla/
// Reddet keyboard, and naming a Turkish filename fixture byte-exact.
// Approving it (surface="telegram", the exact server-side call a real
// button tap makes - kahyad/internal/telegram/approvals.go's
// handleCallback) is then followed by an execution: this test promotes
// fs_write/W2/global to auto-allow (the ONLY sanctioned way to raise a
// ladder level, `kahya autonomy promote` - HANDOFF S4) and re-issues the
// IDENTICAL fs_write call, which now auto-executes end to end through the
// real mcp/fs.Server.HandleWrite (Check -> ConsumeToken -> write ->
// ledger, all in one call) — see the note below this doc comment for why
// this two-step "approve, then separately promote" shape is what actually
// proves execution using ONLY landed code.
//
// NOTE on why promotion, not a token replay: kahyad/internal/policy.
// Engine.Approve mints a fresh one-time token when a pending approval is
// approved, but nothing in the CURRENTLY LANDED system ever hands that
// token back to the ORIGINAL tool call for a resumed retry — mcp/fs's
// HandleWrite always performs its own fresh Check (never accepts a
// pre-approved token as an argument), and a fresh Check for the SAME
// (tool,class,scope) triple is governed by the LADDER LEVEL, not by
// whether some earlier pending id was separately approved. This is a real
// (if narrow) gap in the approve -> execute wiring; it does not undermine
// any safety invariant (the action still can't execute without SOME valid
// decision + a freshly consumed token), so it is noted here rather than
// patched under this commit (see this task's own report for the
// suggestion to wire a resume path in a follow-up task).
func TestGate1_W2RequiresByteExactTelegramDiffApproval(t *testing.T) {
	fake := faketelegram.New()
	t.Cleanup(fake.Close)
	d := bootKahyad(t, daemonOpts{
		telegramURL: fake.URL(), telegramChatID: gateTelegramChatID, telegramUserID: gateTelegramUserID,
	})

	const traceID = "trace-gate1-w2-telegram-0001"
	const taskID = "task-gate1"
	const path = "~/Kahya-w3-gate/evlerimizden-tatil-notlarim.md" // Turkish filename fixture
	content := []byte("Evlerimizden Kadıköy'e kahve içmeye gittik, çok güzeldi; dönüşte vapura yetiştik.")
	args := map[string]any{"path": path, "content_base64": base64.StdEncoding.EncodeToString(content)}

	before := d.existingApprovalIDs(t, "fs_write")
	res := d.mcpCall(t, traceID, taskID, "fs_write", args)
	if !res.IsError {
		t.Fatalf("fs_write at fresh L0 = %+v, want an error (needs approval)", res)
	}

	id := d.newPendingID(t, "fs_write", before)
	rendered := d.approvalDetail(t, id)
	if !strings.Contains(rendered, "evlerimizden-tatil-notlarim.md") {
		t.Fatalf("rendered diff missing the Turkish filename fixture: %s", rendered)
	}
	if !strings.Contains(rendered, "Evlerimizden") {
		t.Fatalf("rendered diff missing the new content: %s", rendered)
	}

	sent := waitForSentMessages(t, fake, 1, 5*time.Second)
	if len(sent) != 1 {
		t.Fatalf("fake telegram transport got %d messages, want exactly 1: %+v", len(sent), sent)
	}
	card := sent[0]
	if card.ChatID != gateTelegramChatID {
		t.Errorf("card chat_id = %d, want %d", card.ChatID, gateTelegramChatID)
	}
	if len(card.Buttons) != 2 {
		t.Fatalf("card buttons = %+v, want exactly 2 (Onayla/Reddet)", card.Buttons)
	}
	if card.Text != rendered {
		t.Fatalf("Telegram card text is NOT byte-exact to the canonical rendering.\n--- telegram ---\n%s\n--- canonical (GET /policy/approvals?id=) ---\n%s", card.Text, rendered)
	}

	// Approve — see this test's own doc comment for why this is driven via
	// POST /policy/feedback directly (surface="telegram") rather than
	// through the fake transport's callback delivery: this is the EXACT
	// call kahyad/internal/telegram/approvals.go's handleCallback makes on
	// a real inline-button tap, and doing it this way recovers the
	// freshly-minted token this test needs. Gate 2's forged-callback
	// subtest independently drives a REAL callback through this SAME fake
	// transport end to end.
	ok, token, errMsg := d.policyFeedback(t, "approve", id, "telegram")
	if !ok || token == "" {
		t.Fatalf("approve(surface=telegram) failed: ok=%v token=%q err=%q", ok, token, errMsg)
	}

	db := d.openDB(t)
	if !waitForEvent(t, db, traceID, "policy_decision", 3*time.Second) {
		t.Fatal("no policy_decision ledger event for this trace_id")
	}
	if !waitForEvent(t, db, traceID, "policy_feedback_approved", 3*time.Second) {
		t.Fatal("no policy_feedback_approved ledger event for this trace_id")
	}
	approvedPayloads := eventPayloads(t, db, traceID, "policy_feedback_approved")
	if len(approvedPayloads) == 0 || !strings.Contains(approvedPayloads[0], `"remote":true`) {
		t.Fatalf("policy_feedback_approved payload = %v, want it to contain remote:true", approvedPayloads)
	}

	// Promote to auto-allow, then re-issue the IDENTICAL fs_write call -
	// this time it Checks ALLOW, consumes a fresh token, and actually
	// writes, all inside mcp/fs.Server.HandleWrite.
	d.promoteToAutoAllow(t, "fs_write", "W2", "global")

	res2 := d.mcpCall(t, traceID, taskID, "fs_write", args)
	if res2.IsError {
		t.Fatalf("fs_write after promotion = %+v, want success", res2)
	}

	writtenPath := filepath.Join(d.homeDir, "Kahya-w3-gate", "evlerimizden-tatil-notlarim.md")
	written, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(written) != string(content) {
		t.Fatalf("written file content = %q, want %q", written, content)
	}

	if !waitForEvent(t, db, traceID, "fs_write", 3*time.Second) {
		t.Fatal("no fs_write (execution) ledger event for this trace_id")
	}
	if n := countEventsMatching(t, db, traceID, "fs_write"); n != 1 {
		t.Fatalf("fs_write ledger events for this trace_id = %d, want exactly 1 (execution happened exactly once)", n)
	}

	// A second attempt to consume the ALREADY-consumed approval id is
	// rejected (single-use) - the one manual approval could not, on its
	// own, be replayed into a second execution.
	if ok2, _, _ := d.policyFeedback(t, "approve", id, "telegram"); ok2 {
		t.Fatal("re-approving the same (already-consumed) pending_approval_id must fail")
	}
}

// ============================================================
// Gate 2 — W3 cannot be approved via Telegram; only local written
// "onayla" ever passes
// ============================================================

// TestGate2_W3NeverApprovedViaTelegramOnlyLocalOnayla is the §6 clause "W3
// eylem Telegram'dan onaylanamıyor, CLI'dan yazılı 'onayla' ile geçiyor".
// One test, three sub-scenarios, ledger rows for each:
//
//  1. a fresh mail_send (W3) pending approval: the fake Telegram transport
//     receives ONLY a notify card (no inline keyboard, ever); a FORGED
//     callback (a synthetically encoded callback_data, from the correct
//     allowlisted chat/user pair - W3 never offered a real button, so ANY
//     callback_data for it is, by definition, not from a genuine tap) is
//     rejected at kahyad/internal/policy.Engine.Approve's own
//     surface="local" backstop, ledgering w3_nonlocal_approval_rejected,
//     and the SAME id remains pending afterward.
//  2. a second mail_send: driving the REAL bin/kahya CLI's `approve <id>`
//     with typed input "evet" is rejected (W3's prompt only accepts the
//     literal word "onayla" - kahyad/cmd/kahya/main.go's runApprove) —
//     the CLI treats this as an explicit DENY, ledgering
//     policy_feedback_denied.
//  3. a third mail_send: the same CLI with typed input "onayla" succeeds,
//     ledgering policy_feedback_approved (surface="local", no "remote"
//     key) — execution (in the sense of "a token now exists to act on")
//     follows ONLY this path, never the "evet" one.
func TestGate2_W3NeverApprovedViaTelegramOnlyLocalOnayla(t *testing.T) {
	root := repoRoot(t)
	kahyaBin := filepath.Join(root, "bin", "kahya")
	requireBuilt(t, kahyaBin)

	fake := faketelegram.New()
	t.Cleanup(fake.Close)
	d := bootKahyad(t, daemonOpts{
		telegramURL: fake.URL(), telegramChatID: gateTelegramChatID, telegramUserID: gateTelegramUserID,
	})
	db := d.openDB(t)

	// --- 1: notify-only card + forged callback rejected ---
	const traceA = "trace-gate2-a-notify-forged-0001"
	beforeA := d.existingApprovalIDs(t, "mail_send")
	resA := d.mcpCall(t, traceA, "task-gate2-a", "mail_send", map[string]any{
		"to": "birisi@ornek.com", "body": "IBAN'a ödeme yapıyorum, lütfen onaylayın.",
	})
	if !resA.IsError {
		t.Fatalf("mail_send (W3) at fresh state = %+v, want an error (needs approval)", resA)
	}
	idA := d.newPendingID(t, "mail_send", beforeA)

	sentA := waitForSentMessages(t, fake, 1, 5*time.Second)
	if len(sentA) != 1 {
		t.Fatalf("fake telegram got %d messages for the W3 notice, want exactly 1: %+v", len(sentA), sentA)
	}
	if len(sentA[0].Buttons) != 0 {
		t.Fatalf("W3 notice must carry NO inline keyboard, got: %+v", sentA[0].Buttons)
	}
	if !strings.Contains(sentA[0].Text, "mail_send") || !strings.Contains(sentA[0].Text, idA) {
		t.Fatalf("W3 notice text = %q, want it to name the tool and the pending id", sentA[0].Text)
	}
	if strings.Contains(sentA[0].Text, "IBAN") || strings.Contains(sentA[0].Text, "ornek.com") {
		t.Fatalf("W3 notice leaked action content: %q", sentA[0].Text)
	}

	forgedData := encodeCallbackData(t, 'A', idA)
	fake.PushCallback(gateTelegramChatID, gateTelegramUserID, forgedData)
	if !waitForEvent(t, db, traceA, "w3_nonlocal_approval_rejected", 5*time.Second) {
		t.Fatal("no w3_nonlocal_approval_rejected ledger event after the forged Telegram callback")
	}
	if waitForEvent(t, db, traceA, "policy_feedback_approved", 500*time.Millisecond) {
		t.Fatal("the forged Telegram callback must NEVER produce a policy_feedback_approved event")
	}
	stillPending := false
	for _, row := range d.listApprovals(t) {
		if row.ID == idA {
			stillPending = true
		}
	}
	if !stillPending {
		t.Fatal("the rejected remote approval attempt must not consume the pending approval id")
	}

	// --- 2: CLI "evet" is rejected (denied) ---
	const traceB = "trace-gate2-b-evet-denied-0001"
	beforeB := d.existingApprovalIDs(t, "mail_send")
	resB := d.mcpCall(t, traceB, "task-gate2-b", "mail_send", map[string]any{
		"to": "ikinci@ornek.com", "body": "test govdesi b",
	})
	if !resB.IsError {
		t.Fatalf("mail_send (W3) = %+v, want an error (needs approval)", resB)
	}
	idB := d.newPendingID(t, "mail_send", beforeB)

	outB, exitB := runKahyaApprove(t, kahyaBin, d.homeDir, d.sockPath, idB, "evet\n")
	if exitB != 1 {
		t.Fatalf("kahya approve %s <<< evet: exit=%d, want 1 (denied)\noutput:\n%s", idB, exitB, outB)
	}
	if !strings.Contains(outB, "Reddedildi.") {
		t.Fatalf("kahya approve %s <<< evet output = %q, want it to contain %q", idB, outB, "Reddedildi.")
	}
	if !waitForEvent(t, db, traceB, "policy_feedback_denied", 3*time.Second) {
		t.Fatal("no policy_feedback_denied ledger event after CLI 'evet' on a W3 prompt")
	}
	if countEventsMatching(t, db, traceB, "policy_feedback_approved") != 0 {
		t.Fatal("CLI 'evet' on a W3 prompt must never approve it")
	}

	// --- 3: CLI "onayla" succeeds (approved) ---
	const traceC = "trace-gate2-c-onayla-approved-0001"
	beforeC := d.existingApprovalIDs(t, "mail_send")
	resC := d.mcpCall(t, traceC, "task-gate2-c", "mail_send", map[string]any{
		"to": "ucuncu@ornek.com", "body": "test govdesi c",
	})
	if !resC.IsError {
		t.Fatalf("mail_send (W3) = %+v, want an error (needs approval)", resC)
	}
	idC := d.newPendingID(t, "mail_send", beforeC)

	outC, exitC := runKahyaApprove(t, kahyaBin, d.homeDir, d.sockPath, idC, "onayla\n")
	if exitC != 0 {
		t.Fatalf("kahya approve %s <<< onayla: exit=%d, want 0 (approved)\noutput:\n%s", idC, exitC, outC)
	}
	if !strings.Contains(outC, "Onaylandı.") {
		t.Fatalf("kahya approve %s <<< onayla output = %q, want it to contain %q", idC, outC, "Onaylandı.")
	}
	if !waitForEvent(t, db, traceC, "policy_feedback_approved", 3*time.Second) {
		t.Fatal("no policy_feedback_approved ledger event after CLI 'onayla'")
	}
	approvedPayloads := eventPayloads(t, db, traceC, "policy_feedback_approved")
	if len(approvedPayloads) == 0 || strings.Contains(approvedPayloads[0], `"remote"`) {
		t.Fatalf("local onayla approval payload = %v, want surface=local with NO remote key", approvedPayloads)
	}
	if !strings.Contains(approvedPayloads[0], `"surface":"local"`) {
		t.Fatalf("local onayla approval payload = %v, want surface:\"local\"", approvedPayloads)
	}
}

// runKahyaApprove runs `bin/kahya approve <id>` against the daemon at
// sockPath with stdin, returning combined stdout+stderr and the exit code.
func runKahyaApprove(t *testing.T, kahyaBin, homeDir, sockPath, id, stdin string) (output string, exitCode int) {
	t.Helper()
	cmd := exec.Command(kahyaBin, "approve", id)
	cmd.Env = buildCLIEnv(homeDir, sockPath)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run kahya approve %s: %v\noutput:\n%s", id, err, out)
		}
	}
	return string(out), code
}

// ============================================================
// Gate 3 — secret-lane-touching actions fall to local approval
// ============================================================

// TestGate3_SecretLaneContentRoutesToLocalApprovalOnly is the §6 clause
// "gizli-şerit dokunuşlu eylemler yerel onaya düşüyor". The fixture
// content ("IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı",
// this task's own spec, verbatim) matches kahyad/internal/secretlane's
// DETERMINISTIC pre-pass — no live Qwen/MLX model is needed to classify
// it. The target PATH deliberately matches none of the fixture policy's
// (empty) secret_lane_globs, so only CONTENT can be driving what follows -
// this exercises this task's own fixes: kahyad/internal/telegram/
// redact.go's isSecretLane and mcp/fs.Server.ContentClassifier now both
// consult kahyad/internal/secretlane.ClassifyDeterministic, not just a
// path glob (see gate_test.go's package doc comment / this task's report
// for why that gap was real and where it is fixed).
func TestGate3_SecretLaneContentRoutesToLocalApprovalOnly(t *testing.T) {
	fake := faketelegram.New()
	t.Cleanup(fake.Close)
	d := bootKahyad(t, daemonOpts{
		telegramURL: fake.URL(), telegramChatID: gateTelegramChatID, telegramUserID: gateTelegramUserID,
	})

	const traceID = "trace-gate3-secretlane-0001"
	const taskID = "task-gate3"
	const path = "~/Kahya-w3-gate/odeme-notu-sıradan-isim.md" // ordinary-looking name/path
	content := []byte("IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı")
	args := map[string]any{"path": path, "content_base64": base64.StdEncoding.EncodeToString(content)}

	beforeWrite := d.existingApprovalIDs(t, "fs_write")
	res := d.mcpCall(t, traceID, taskID, "fs_write", args)
	if !res.IsError {
		t.Fatalf("fs_write at fresh L0 = %+v, want an error (needs approval)", res)
	}
	id := d.newPendingID(t, "fs_write", beforeWrite)

	// Telegram gets AT MOST a redacted title: no keyboard, no diff, no
	// IBAN bytes, no path.
	sent := waitForSentMessages(t, fake, 1, 5*time.Second)
	if len(sent) != 1 {
		t.Fatalf("fake telegram got %d messages, want exactly 1: %+v", len(sent), sent)
	}
	if len(sent[0].Buttons) != 0 {
		t.Fatalf("secret-lane card must never carry an approval keyboard, got: %+v", sent[0].Buttons)
	}
	for _, leak := range []string{"IBAN", "TR33", "6457", "odeme-notu"} {
		if strings.Contains(sent[0].Text, leak) {
			t.Fatalf("secret-lane payload bytes leaked to Telegram (%q found): %q", leak, sent[0].Text)
		}
	}
	if !strings.Contains(sent[0].Text, "🔒") {
		t.Fatalf("secret-lane notice text = %q, want the 🔒 redaction marker", sent[0].Text)
	}

	// The LOCAL surface (GET /policy/approvals?id=, the same data `kahya
	// approve <id>` renders) still shows the FULL real diff — the
	// redaction is Telegram-presentation-only, never a loss of the
	// underlying approval data ("routes to the CLI surface").
	rendered := d.approvalDetail(t, id)
	if !strings.Contains(rendered, "IBAN TR33") {
		t.Fatalf("local approval surface must still render the full diff, got: %s", rendered)
	}

	// Promote + re-issue so the file actually lands, then read it back to
	// trigger the (separately fixed) content-based sensitive-read mark.
	d.promoteToAutoAllow(t, "fs_write", "W2", "global")
	res2 := d.mcpCall(t, traceID, taskID, "fs_write", args)
	if res2.IsError {
		t.Fatalf("fs_write after promotion = %+v, want success", res2)
	}

	d.promoteToAutoAllow(t, "fs_read", "R", "global")
	res3 := d.mcpCall(t, traceID, taskID, "fs_read", map[string]any{"path": path})
	if res3.IsError {
		t.Fatalf("fs_read after promotion = %+v, want success", res3)
	}
	var readOut struct {
		SecretLane bool `json:"secret_lane"`
	}
	if err := json.Unmarshal([]byte(res3.Text), &readOut); err != nil {
		t.Fatalf("decode fs_read output %q: %v", res3.Text, err)
	}
	if !readOut.SecretLane {
		t.Fatal("fs_read did not classify the IBAN content as secret-lane (mcp/fs.Server.ContentClassifier)")
	}

	db := d.openDB(t)
	if !waitForEvent(t, db, traceID, "secret_lane_read", 3*time.Second) {
		t.Fatal("no secret_lane_read ledger event")
	}
	if !waitForEvent(t, db, traceID, "sensitive_read_marked", 3*time.Second) {
		t.Fatal("the session's sensitive-read flag was never set (no sensitive_read_marked ledger event)")
	}
}

// ============================================================
// Gate 4 — in-container curl cannot bypass the egress allowlist (Docker)
// ============================================================

// TestGate4_DockerCurlCannotBypassEgressAllowlist is the §6 clause
// "container içi curl allowlist'i atlayamıyor (test)". Unlike mcp/shell's
// own egress_integration_test.go (which cannot import kahyad/internal/
// egress and so drives a small package-external stand-in proxy), this
// test runs shell_docker through the REAL, running child kahyad's own
// /v1/mcp — the SAME kahyad/internal/egress.Gate/Proxy the whole daemon
// uses — so its egress_blocked_* ledger rows are the real thing, not a
// stand-in's. Requires Docker (KAHYA_DOCKER_TESTS=1); SKIPS, never fails,
// when unset.
func TestGate4_DockerCurlCannotBypassEgressAllowlist(t *testing.T) {
	requireDockerTests(t)

	// mcp/shell.EgressNetworkEnsurer treats an already-"running"
	// kahya-egress-fwd sidecar container as already-ensured and skips
	// recreating it - which would leave it pointed at a PRIOR run's own
	// (freeTCPPort-randomized, per this test's own daemonOpts) egress port
	// forever, exactly the staleness mcp/shell/egress_integration_test.go's
	// own t.Cleanup already guards against for its own runs. Remove any
	// leftover from a previous run of THIS test before starting, and clean
	// up after this run too.
	removeStaleEgressSidecar(t)
	t.Cleanup(func() { removeStaleEgressSidecar(t) })

	root := repoRoot(t)
	sandboxDigest := readDigestFile(t, filepath.Join(root, "docker", "sandbox", "IMAGE_DIGEST"))
	egressDigest := readDigestFile(t, filepath.Join(root, "docker", "egress", "IMAGE_DIGEST"))
	if sandboxDigest == "" {
		t.Fatal("docker/sandbox/IMAGE_DIGEST is empty - run `make sandbox-image` first")
	}
	if egressDigest == "" {
		t.Fatal("docker/egress/IMAGE_DIGEST is empty - the egress sidecar image is not pinned")
	}

	d := bootKahyad(t, daemonOpts{})
	d.promoteToAutoAllow(t, "shell_docker", "W2", "global")

	// --- (a) needs_network:true: proxy-403 / direct-IP / DNS bypass vectors ---
	const traceNet = "trace-gate4-egress-net-0001"
	netScript := `
FAIL=0

echo "=== step1: non-allowlisted host via the proxy must be 403 ==="
OUT1=$(curl -s -v --max-time 10 https://example.com/ 2>&1)
if echo "$OUT1" | grep -q "403"; then
  echo STEP1_OK_403_DENIED
else
  echo "STEP1_FAIL: $OUT1"
  FAIL=1
fi

echo "=== step2: direct-IP bypass (--noproxy) must be unreachable ==="
if curl -s --max-time 5 --noproxy '*' https://1.1.1.1 >/dev/null 2>&1; then
  echo STEP2_FAIL_REACHABLE
  FAIL=1
else
  echo STEP2_OK_UNREACHABLE
fi

echo "=== step3: DNS resolution of an external name must fail ==="
if getent hosts example.com >/dev/null 2>&1; then
  echo STEP3_FAIL_RESOLVED
  FAIL=1
else
  echo STEP3_OK_UNRESOLVED
fi

exit $FAIL
`
	netRes := d.mcpCall(t, traceNet, "task-gate4-net", "shell_docker", map[string]any{
		"script": netScript, "workdir": t.TempDir(), "timeout_s": 30, "needs_network": true,
	})
	if netRes.IsError {
		t.Fatalf("shell_docker (needs_network) call itself failed: %s", netRes.Text)
	}
	var netOut shellDockerOutput
	if err := json.Unmarshal([]byte(netRes.Text), &netOut); err != nil {
		t.Fatalf("decode shell_docker output: %v\ntext=%s", err, netRes.Text)
	}
	t.Logf("gate4 needs_network script stdout:\n%s", netOut.Stdout)
	if netOut.ExitCode != 0 {
		t.Fatalf("needs_network containment script exited %d (want 0)\nstdout=%s\nstderr=%s", netOut.ExitCode, netOut.Stdout, netOut.Stderr)
	}
	for _, want := range []string{"STEP1_OK_403_DENIED", "STEP2_OK_UNREACHABLE", "STEP3_OK_UNRESOLVED"} {
		if !strings.Contains(netOut.Stdout, want) {
			t.Errorf("expected stdout to contain %q:\n%s", want, netOut.Stdout)
		}
	}

	db := d.openDB(t)
	if !waitForEvent(t, db, traceNet, "egress_blocked_allowlist", 5*time.Second) {
		t.Fatal("no egress_blocked_allowlist ledger event for the non-allowlisted-host attempt")
	}

	// --- (b) default --network none: ALL egress fails ---
	const traceNoNet = "trace-gate4-egress-nonet-0001"
	noNetScript := `
FAIL=0
if curl -s --max-time 5 https://example.com/ >/dev/null 2>&1; then
  echo STEP4_FAIL_REACHABLE
  FAIL=1
else
  echo STEP4_OK_UNREACHABLE
fi
if getent hosts example.com >/dev/null 2>&1; then
  echo STEP5_FAIL_RESOLVED
  FAIL=1
else
  echo STEP5_OK_UNRESOLVED
fi
exit $FAIL
`
	noNetRes := d.mcpCall(t, traceNoNet, "task-gate4-nonet", "shell_docker", map[string]any{
		"script": noNetScript, "workdir": t.TempDir(), "timeout_s": 30,
	})
	if noNetRes.IsError {
		t.Fatalf("shell_docker (default network) call itself failed: %s", noNetRes.Text)
	}
	var noNetOut shellDockerOutput
	if err := json.Unmarshal([]byte(noNetRes.Text), &noNetOut); err != nil {
		t.Fatalf("decode shell_docker output: %v\ntext=%s", err, noNetRes.Text)
	}
	t.Logf("gate4 default-network script stdout:\n%s", noNetOut.Stdout)
	if noNetOut.ExitCode != 0 {
		t.Fatalf("--network none containment script exited %d (want 0)\nstdout=%s\nstderr=%s", noNetOut.ExitCode, noNetOut.Stdout, noNetOut.Stderr)
	}
	for _, want := range []string{"STEP4_OK_UNREACHABLE", "STEP5_OK_UNRESOLVED"} {
		if !strings.Contains(noNetOut.Stdout, want) {
			t.Errorf("expected stdout to contain %q:\n%s", want, noNetOut.Stdout)
		}
	}
}

// egressSidecarName mirrors mcp/shell.EgressSidecarName's literal value
// ("kahya-egress-fwd") - imported directly would be fine too (mcp/shell is
// NOT under kahyad/internal/, freely importable here), but this file
// avoids taking a whole extra import for one constant string used in
// exactly one cleanup helper.
const egressSidecarName = "kahya-egress-fwd"

func removeStaleEgressSidecar(t *testing.T) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", egressSidecarName).Run()
}

type shellDockerOutput struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func readDigestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ============================================================
// Gate 5 — secret-lane content cannot reach a cloud call
// ============================================================

// TestGate5_SecretLaneCannotReachCloud is the §6 clause "gizli-şerit
// içerik bulut çağrısına çıkamıyor (test)", in its two required forms.
func TestGate5_SecretLaneCannotReachCloud(t *testing.T) {
	root := repoRoot(t)
	pythonBin := "python3"

	t.Run("proxy_backstop_blocks_after_task_marked_secret_lane", func(t *testing.T) {
		mock := mockanthropic.New()
		t.Cleanup(mock.Close)

		signalFile := filepath.Join(t.TempDir(), "gate5.signal")
		outFile := filepath.Join(t.TempDir(), "gate5.out")
		workerCmd := []string{pythonBin, filepath.Join(root, "tests", "w3", "fixtures", "gate5_worker.py")}

		d := bootKahyad(t, daemonOpts{
			anthropicUpstreamURL: mock.URL(),
			workerCmd:            workerCmd,
			extraEnv: []string{
				"KAHYA_GATE5_SIGNAL_FILE=" + signalFile,
				"KAHYA_GATE5_OUT_FILE=" + outFile,
			},
		})

		const traceID = "trace-gate5-backstop-0001"
		// An innocuous prompt: the deterministic classifier must NOT flag
		// this at task-creation time (lane stays "normal", so a real
		// per-task anthproxy.Proxy listener + worker are actually spawned -
		// this test then marks the task secret-lane WHILE that worker is
		// alive and waiting, exactly mirroring task.go's own "second,
		// independent layer of defense in case [the lane==secret skip at
		// creation] ever changes" framing).
		resp := d.postTask(t, traceID, "Yarın hava nasıl olacak, dışarı çıkabilir miyim?")
		drainSSEAsync(resp)

		taskID := waitForTaskID(t, d, traceID, 5*time.Second)
		setTaskLaneSecret(t, d, taskID, "finans")

		if err := os.WriteFile(signalFile, []byte("go"), 0o600); err != nil {
			t.Fatalf("write signal file: %v", err)
		}

		outBody := waitForFileContent(t, outFile, 15*time.Second)
		if !strings.HasPrefix(outBody, "STATUS:403") {
			t.Fatalf("gate5_worker's direct call result = %q, want STATUS:403 prefix", outBody)
		}
		if !strings.Contains(outBody, "gizli şerit") {
			t.Errorf("blocked response body = %q, want it to contain the Turkish secretlane_cloud_blocked message", outBody)
		}

		db := d.openDB(t)
		if !waitForEvent(t, db, traceID, "secretlane_cloud_blocked", 3*time.Second) {
			t.Fatal("no secretlane_cloud_blocked ledger event")
		}
		if got := len(mock.Requests()); got != 0 {
			t.Fatalf("mock Anthropic upstream recorded %d requests for this trace_id's task, want 0", got)
		}
	})

	// t.Run("hanging_classifier..."): DEVIATION, documented (see this
	// subtest's own doc comment below) — POST /v1/task's own prompt
	// classification (kahyad/internal/server/task.go) deliberately calls
	// ONLY secretlane.ClassifyDeterministic, never the Qwen-backed
	// s.secretLaneClassifier, by an explicit, already-documented W3-08
	// design decision (task.go's own "SCOPE DECISION (post-review,
	// explicit and deliberate — not a gap)" comment: a user's directly-
	// typed chat prompt is never routed through the full/Qwen-fallback
	// classifier — only the three W4-03 ingestion points — memory_write,
	// fs reads flagged for model consumption, mail/web Reader input — are
	// meant to, and none of those are wired to a live caller yet). There
	// is consequently NO currently-wired HTTP-reachable call site where
	// "classification is hanging on Qwen" is an observable state at all —
	// confirmed empirically: forcing a non-deterministic prompt through
	// POST /v1/task with a genuinely hanging Qwen stand-in wired in still
	// completes in well under a second, because classification here never
	// touches Qwen. The classifier-level ordering invariant itself
	// (hanging Qwen ⇒ zero proxy bytes) is already covered by permanent
	// tests in kahyad/internal/secretlane (TestClassifyBlocksOnHangingQwenUntilCtxDone,
	// TestOrderingInvariantHangingClassifierProducesZeroProxyBytes), which
	// `make test` already runs — this package cannot import that one
	// (kahyad/internal/* import boundary, this file's own package doc
	// comment), so it cannot be re-run verbatim from here.
	//
	// This subtest instead exercises the CLOSEST currently-wired
	// equivalent that a real HTTP call CAN reach: a task the deterministic
	// pre-pass has ALREADY, instantly, and correctly labeled secret-lane
	// (the IBAN fixture) is answered ENTIRELY by the local Qwen-backed
	// secretlane.Answerer (kahyad/internal/secretlane/answer.go) — wired
	// to the SAME hanging Qwen stand-in — proving the ordering invariant's
	// real consequence end to end: even while that local step is
	// genuinely stuck (never completing within this subtest's own
	// generous wait), zero bytes have reached the cloud stand-in, because
	// a secret-lane task's code path never opens a worker/proxy toward it
	// AT ALL (task.go's own "no envelope, no task row, no worker/proxy for
	// this task to exist yet at all" framing) — hanging or not.
	t.Run("secret_lane_answer_hangs_on_local_model_never_reaches_cloud", func(t *testing.T) {
		mock := mockanthropic.New()
		t.Cleanup(mock.Close)

		qwenCmd := []string{pythonBin, filepath.Join(root, "tests", "w3", "fixtures", "hanging_qwen.py")}
		d := bootKahyad(t, daemonOpts{
			anthropicUpstreamURL: mock.URL(),
			qwenCmd:              qwenCmd,
			qwenModelPath:        "unused-hanging-qwen-fixture-path",
		})

		const traceID = "trace-gate5-hanging-0001"
		const prompt = "IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı"

		resp := d.postTask(t, traceID, prompt)
		drainSSEAsync(resp)

		// The SAME deterministic pre-pass Gate 5's first subtest already
		// proved fires instantly ledgers secretlane_classified/task_spawned
		// within milliseconds; the local ANSWER step is what is genuinely
		// stuck on the hanging Qwen stand-in for the whole of this wait.
		db := d.openDB(t)
		if !waitForEvent(t, db, traceID, "task_spawned", 3*time.Second) {
			t.Fatal("no task_spawned ledger event - the task never even started")
		}
		if countEventsMatching(t, db, traceID, "task_done") != 0 {
			t.Fatal("task_done fired before the (deliberately hanging) local answer step could have completed - the fixture stopped hanging")
		}
		time.Sleep(2 * time.Second)
		if got := len(mock.Requests()); got != 0 {
			t.Fatalf("mock Anthropic upstream recorded %d requests while the secret-lane task's local answer was still in flight, want 0 (ordering invariant)", got)
		}
		if countEventsMatching(t, db, traceID, "task_done") != 0 {
			t.Fatal("task_done fired during the wait window - the local answer step did not actually hang as this fixture requires")
		}
	})
}
