package fs

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ---- fakes (mirrors mcp/memory/server_test.go's fakeLedger convention). ----

type fakeLedgerEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

type fakeLedger struct {
	mu     sync.Mutex
	events []fakeLedgerEvent
}

func (f *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeLedgerEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeLedger) find(kind string) []fakeLedgerEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeLedgerEvent
	for _, e := range f.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// fakePolicyClient simulates kahyad/internal/policy.Engine's Check/
// ConsumeToken pair closely enough for this package's own unit tests:
// Check always returns `decision` (or, if resultByTool is set, a
// per-tool override) unless checkErr is set; ConsumeToken enforces
// real single-use semantics against consumedTokens, so a token-replay
// test exercises the SAME failure mode the real engine would produce.
type fakePolicyClient struct {
	mu             sync.Mutex
	decision       PolicyDecision
	resultByTool   map[string]PolicyDecision
	checkErr       error
	consumeErr     error
	consumedTokens map[string]bool
	checkCalls     int
	consumeCalls   int
}

func (f *fakePolicyClient) Check(_ context.Context, tool, _ /* scope */, _ /* taskID */, _ /* traceID */ string, _ []byte) (PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checkErr != nil {
		return PolicyDecision{}, f.checkErr
	}
	if d, ok := f.resultByTool[tool]; ok {
		return d, nil
	}
	return f.decision, nil
}

func (f *fakePolicyClient) ConsumeToken(_ context.Context, token, _ /* tool */, _ /* class */, _ /* scope */, _ /* taskID */, _ /* traceID */ string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeCalls++
	if f.consumeErr != nil {
		return f.consumeErr
	}
	if f.consumedTokens == nil {
		f.consumedTokens = make(map[string]bool)
	}
	if f.consumedTokens[token] {
		return errors.New("policy: approval token invalid, expired, or already consumed")
	}
	f.consumedTokens[token] = true
	return nil
}

// allowDecision is the common "ladder auto-allowed, W1 token minted"
// shape HandleWrite/HandleDelete need to proceed past the policy gate.
func allowDecision(token string) PolicyDecision {
	return PolicyDecision{Result: PolicyResultAllow, Class: "W1", Token: token}
}

func newTestServer(t *testing.T, home string, denyGlobs, secretLaneGlobs []string, pc PolicyClient, led Ledger) *Server {
	t.Helper()
	s := New(home, denyGlobs, secretLaneGlobs, filepath.Join(home, "undo"), pc, led, nil)
	return s
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// ---- fs_read ----

func TestHandleReadReturnsContentAndMetadata(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notes", "a.md"), "merhaba dunya")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	out, err := s.HandleRead(context.Background(), "trace-1", FsReadArgs{Path: "~/notes/a.md"})
	if err != nil {
		t.Fatalf("HandleRead: %v", err)
	}
	if out.Size != int64(len("merhaba dunya")) {
		t.Errorf("Size = %d, want %d", out.Size, len("merhaba dunya"))
	}
	decoded, err := base64.StdEncoding.DecodeString(out.ContentBase64)
	if err != nil || string(decoded) != "merhaba dunya" {
		t.Errorf("ContentBase64 decodes to %q (err=%v), want %q", decoded, err, "merhaba dunya")
	}
	if out.SecretLane {
		t.Error("SecretLane = true, want false (no secret_lane_globs configured)")
	}
	if len(led.find("fs_read")) != 1 {
		t.Errorf("fs_read ledger events = %d, want 1", len(led.find("fs_read")))
	}
}

func TestHandleReadNeedsApprovalFailsAndNeverTouchesDisk(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "notes", "a.md")
	mustWriteFile(t, target, "content")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "Bu eylem icin onay gerekiyor."}}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	if _, err := s.HandleRead(context.Background(), "trace-1", FsReadArgs{Path: "~/notes/a.md"}); err == nil {
		t.Fatal("HandleRead with NEEDS_APPROVAL decision = nil error, want error")
	}
}

// TestHandleReadSecretLaneMarksLedgerAndMetadata is the task's own
// acceptance-criteria fixture, byte-exact: ~/Documents/saglik/tahlil-sonuçları.pdf.
func TestHandleReadSecretLaneMarksLedgerAndMetadata(t *testing.T) {
	home := testHome(t)
	rel := "Documents/saglik/tahlil-sonuçları.pdf"
	mustWriteFile(t, filepath.Join(home, filepath.FromSlash(rel)), "gizli tahlil verisi")

	secretLaneGlobs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, secretLaneGlobs, pc, led)

	out, err := s.HandleRead(context.Background(), "trace-secret", FsReadArgs{Path: "~/Documents/saglik/tahlil-sonuçları.pdf", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("HandleRead: %v", err)
	}
	if !out.SecretLane {
		t.Error("SecretLane = false, want true for a secret_lane_globs match")
	}

	events := led.find("secret_lane_read")
	if len(events) != 1 {
		t.Fatalf("secret_lane_read ledger events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.traceID != "trace-secret" {
		t.Errorf("trace_id = %q, want %q", ev.traceID, "trace-secret")
	}
	if ev.payload["session_id"] != "sess-1" {
		t.Errorf("session_id = %v, want %q", ev.payload["session_id"], "sess-1")
	}
}

func TestHandleReadFullDiskAccessErrorOnPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: POSIX permission bits are bypassed, cannot exercise EPERM/EACCES")
	}
	home := testHome(t)
	target := filepath.Join(home, "locked.txt")
	mustWriteFile(t, target, "secret")
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer os.Chmod(target, 0o600)

	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	_, err := s.HandleRead(context.Background(), "trace-1", FsReadArgs{Path: "~/locked.txt"})
	if err == nil {
		t.Skip("read of a chmod-0000 file unexpectedly succeeded on this platform/user; skipping")
	}
	var fdaErr *FullDiskAccessError
	if !errors.As(err, &fdaErr) {
		t.Fatalf("HandleRead error = %v (%T), want *FullDiskAccessError", err, err)
	}
	if want := "Tam Disk Erişimi gerekli: " + fdaErr.Path; fdaErr.Error() != want {
		t.Errorf("Error() = %q, want %q", fdaErr.Error(), want)
	}
}

// ---- fs_write: deny globs ----

func TestHandleWriteDenyGlobHitDeniesBeforeAnyPolicyCheck(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, ".zshrc"), "original")
	pc := &fakePolicyClient{decision: allowDecision("shouldneverbeused")}
	led := &fakeLedger{}
	s := newTestServer(t, home, []string{filepath.Join(home, ".zshrc")}, nil, pc, led)

	_, err := s.HandleWrite(context.Background(), "trace-deny", "task-1", FsWriteArgs{Path: "~/.zshrc", ContentBase64: b64("evil")})
	if err == nil {
		t.Fatal("HandleWrite to a denied path = nil error, want error")
	}
	if pc.checkCalls != 0 {
		t.Errorf("Policy.Check was called %d times, want 0 — deny-glob must short-circuit BEFORE any policy decision", pc.checkCalls)
	}
	if got := len(led.find("fs_deny_glob_hit")); got != 1 {
		t.Fatalf("fs_deny_glob_hit ledger events = %d, want 1", got)
	}
	content, rerr := os.ReadFile(filepath.Join(home, ".zshrc"))
	if rerr != nil || string(content) != "original" {
		t.Errorf(".zshrc content = %q (err=%v), want unchanged %q", content, rerr, "original")
	}
}

func TestHandleDeleteDenyGlobHitAppliesSameGateChain(t *testing.T) {
	home := testHome(t)
	mustMkdirAll(t, filepath.Join(home, "Library", "LaunchAgents"))
	target := filepath.Join(home, "Library", "LaunchAgents", "evil.plist")
	mustWriteFile(t, target, "payload")
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	led := &fakeLedger{}
	s := newTestServer(t, home, []string{filepath.Join(home, "Library", "LaunchAgents", "**")}, nil, pc, led)

	_, err := s.HandleDelete(context.Background(), "trace-deny-del", "task-1", FsDeleteArgs{Path: "~/Library/LaunchAgents/evil.plist"})
	if err == nil {
		t.Fatal("HandleDelete of a denied path = nil error, want error")
	}
	if pc.checkCalls != 0 {
		t.Errorf("Policy.Check was called %d times, want 0", pc.checkCalls)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("target file was removed despite deny-glob hit: %v", statErr)
	}
}

func TestHandleWriteCaseInsensitiveDenyGlobHit(t *testing.T) {
	home := testHome(t)
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	s := newTestServer(t, home, []string{filepath.Join(home, "Library", "LaunchAgents", "**")}, nil, pc, &fakeLedger{})

	_, err := s.HandleWrite(context.Background(), "trace-ci", "task-1", FsWriteArgs{Path: "~/LIBRARY/LaunchAgents/x.plist", ContentBase64: b64("evil")})
	if err == nil {
		t.Fatal("HandleWrite to an upper-cased denied path = nil error, want error (case-insensitive deny match)")
	}
}

// ---- fs_write: needs_approval / token replay ----

func TestHandleWriteNeedsApprovalFailsWithoutToken(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "scratch.txt")
	mustWriteFile(t, target, "original")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "Bu eylem icin onay gerekiyor."}}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	_, err := s.HandleWrite(context.Background(), "trace-na", "task-1", FsWriteArgs{Path: "~/scratch.txt", ContentBase64: b64("new content")})
	if err == nil {
		t.Fatal("HandleWrite with NEEDS_APPROVAL decision = nil error, want error")
	}
	if pc.consumeCalls != 0 {
		t.Errorf("ConsumeToken was called %d times, want 0", pc.consumeCalls)
	}
	content, rerr := os.ReadFile(target)
	if rerr != nil || string(content) != "original" {
		t.Errorf("file content = %q (err=%v), want unchanged %q", content, rerr, "original")
	}
	if got := len(led.find("fs_write")); got != 0 {
		t.Errorf("fs_write ledger events = %d, want 0 (write never happened)", got)
	}
}

func TestHandleWriteTokenReplayFails(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "scratch.txt")
	mustWriteFile(t, target, "v1")
	pc := &fakePolicyClient{decision: allowDecision("fixed-token-reused")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	if _, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/scratch.txt", ContentBase64: b64("v2")}); err != nil {
		t.Fatalf("first HandleWrite: %v", err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "v2" {
		t.Fatalf("after first write, content = %q, want %q", content, "v2")
	}

	// Second call: Check() again returns the SAME (already-consumed) token
	// — simulating a replay attempt. ConsumeToken must fail, and the
	// second write's bytes must never land.
	_, err := s.HandleWrite(context.Background(), "trace-2", "task-1", FsWriteArgs{Path: "~/scratch.txt", ContentBase64: b64("v3-should-not-land")})
	if err == nil {
		t.Fatal("second HandleWrite reusing a consumed token = nil error, want error (replay)")
	}
	content, _ = os.ReadFile(target)
	if string(content) != "v2" {
		t.Errorf("after replayed write attempt, content = %q, want unchanged %q", content, "v2")
	}
}
