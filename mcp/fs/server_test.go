package fs

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	// checkTaskIDs records the taskID of every Check call, in order.
	checkTaskIDs []string
	// onCheck, if set, runs once per Check call, standing in for the
	// wall-clock window a REAL PolicyClient.Check (a DB round trip) plus
	// the ConsumeToken call and checkpointPreImage's own git exec that
	// follow it in HandleWrite/HandleDelete would otherwise represent —
	// the TOCTOU regression test below uses this to plant a symlink
	// "during" that window, after canonicalize/deny-glob already ran but
	// before the actual mutation.
	onCheck func()
}

func (f *fakePolicyClient) Check(_ context.Context, tool, _ /* scope */, taskID, _ /* traceID */ string, _ []byte) (PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	f.checkTaskIDs = append(f.checkTaskIDs, taskID)
	if f.onCheck != nil {
		f.onCheck()
	}
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
	s := New(home, denyGlobs, secretLaneGlobs, filepath.Join(home, "undo"), pc, led, nil, nil)
	return s
}

// fakeSensitiveMarker is a spy SensitiveReadMarker (W3-05): records every
// call so a test can assert fs_read's secret_lane_read seam calls it
// exactly when expected.
type fakeSensitiveMarker struct {
	mu    sync.Mutex
	calls []struct{ sessionID, traceID string }
	err   error
}

func (f *fakeSensitiveMarker) MarkSensitiveRead(_ context.Context, sessionID, traceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ sessionID, traceID string }{sessionID, traceID})
	return f.err
}

func (f *fakeSensitiveMarker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeLaneEscalator is a spy LaneEscalator (FIX-B-6).
type fakeLaneEscalator struct {
	mu    sync.Mutex
	calls []escalateCall
	err   error
}

type escalateCall struct{ taskID, traceID, category string }

func (f *fakeLaneEscalator) EscalateSecretLane(_ context.Context, taskID, traceID, category string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, escalateCall{taskID, traceID, category})
	return f.err
}

func (f *fakeLaneEscalator) snapshot() []escalateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]escalateCall(nil), f.calls...)
}

// fakeContentClassifier is a fixed-verdict ContentClassifier.
type fakeContentClassifier struct {
	secret   bool
	category string
}

func (f fakeContentClassifier) ClassifySecretLane(string) (bool, string) { return f.secret, f.category }

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

// TestHandleReadForTaskPassesTaskIDToPolicyCheck: fs_read's binding check
// carries the calling task (B-1: the pending approval, its grant, a halt
// and the approval notice are keyed on it); the task-less HandleRead keeps
// passing "".
func TestHandleReadForTaskPassesTaskIDToPolicyCheck(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notes", "a.md"), "merhaba dunya")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	if _, err := s.HandleReadForTask(context.Background(), "trace-1", "task-1", FsReadArgs{Path: "~/notes/a.md"}); err != nil {
		t.Fatalf("HandleReadForTask: %v", err)
	}
	if _, err := s.HandleRead(context.Background(), "trace-2", FsReadArgs{Path: "~/notes/a.md"}); err != nil {
		t.Fatalf("HandleRead: %v", err)
	}
	if want := []string{"task-1", ""}; !reflect.DeepEqual(pc.checkTaskIDs, want) {
		t.Fatalf("Policy.Check task ids = %q, want %q", pc.checkTaskIDs, want)
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

	out, err := s.HandleRead(context.Background(), "trace-secret", FsReadArgs{Path: "~/Documents/saglik/tahlil-sonuçları.pdf"})
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
	// BLOCKER A fix: session_id is now the REQUEST's own trace_id (there
	// is no model-supplied session_id argument left at all — see
	// FsReadArgs' doc comment).
	if ev.payload["session_id"] != "trace-secret" {
		t.Errorf("session_id = %v, want %q", ev.payload["session_id"], "trace-secret")
	}
}

// TestHandleReadSecretLaneCallsSensitiveMarker is W3-05's own seam test:
// a secret_lane_globs hit calls SensitiveMarker.MarkSensitiveRead exactly
// once, keyed on the request's own trace_id (BLOCKER A fix — FsReadArgs
// has no session_id argument at all anymore; see its doc comment).
func TestHandleReadSecretLaneCallsSensitiveMarker(t *testing.T) {
	home := testHome(t)
	rel := "Documents/saglik/tahlil-sonuçları.pdf"
	mustWriteFile(t, filepath.Join(home, filepath.FromSlash(rel)), "gizli tahlil verisi")

	secretLaneGlobs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	marker := &fakeSensitiveMarker{}
	s := New(home, nil, secretLaneGlobs, filepath.Join(home, "undo"), pc, led, nil, marker)

	if _, err := s.HandleRead(context.Background(), "trace-mark", FsReadArgs{
		Path: "~/Documents/saglik/tahlil-sonuçları.pdf",
	}); err != nil {
		t.Fatalf("HandleRead: %v", err)
	}

	if marker.callCount() != 1 {
		t.Fatalf("MarkSensitiveRead calls = %d, want 1", marker.callCount())
	}
	got := marker.calls[0]
	if got.sessionID != "trace-mark" || got.traceID != "trace-mark" {
		t.Errorf("MarkSensitiveRead called with (%q, %q), want (%q, %q) — taint keyed on the REQUEST trace_id", got.sessionID, got.traceID, "trace-mark", "trace-mark")
	}
}

// TestHandleReadSecretLaneAlwaysMarksTaintOnRequestTraceID is the BLOCKER
// A regression test (replaces the old
// TestHandleReadSecretLaneSkipsMarkerWithoutSessionID, which proved the
// OPPOSITE of what is now correct behavior): FsReadArgs no longer has a
// session_id field at all, so there is NO argument the model could omit
// or forge to suppress or redirect the taint mark. A secret-lane read
// ALWAYS marks taint, keyed on the request's own SERVER-ASSIGNED
// trace_id — proving the model cannot suppress or redirect it.
func TestHandleReadSecretLaneAlwaysMarksTaintOnRequestTraceID(t *testing.T) {
	home := testHome(t)
	rel := "Documents/saglik/tahlil-sonuçları.pdf"
	mustWriteFile(t, filepath.Join(home, filepath.FromSlash(rel)), "gizli tahlil verisi")

	secretLaneGlobs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	marker := &fakeSensitiveMarker{}
	s := New(home, nil, secretLaneGlobs, filepath.Join(home, "undo"), pc, led, nil, marker)

	if _, err := s.HandleRead(context.Background(), "trace-nomark", FsReadArgs{
		Path: "~/Documents/saglik/tahlil-sonuçları.pdf",
	}); err != nil {
		t.Fatalf("HandleRead: %v", err)
	}
	if marker.callCount() != 1 {
		t.Fatalf("MarkSensitiveRead calls = %d, want 1 (taint is unconditional now — no model-controlled argument can skip it)", marker.callCount())
	}
	got := marker.calls[0]
	if got.sessionID != "trace-nomark" || got.traceID != "trace-nomark" {
		t.Errorf("MarkSensitiveRead called with (%q, %q), want (%q, %q)", got.sessionID, got.traceID, "trace-nomark", "trace-nomark")
	}
}

// TestHandleReadSecretLaneSensitiveMarkFailureRefusesRead is FIX-B-7's
// fail-closed half at the fs_read seam: when the durable sensitive-read
// mark cannot be persisted, the read is refused with the Turkish
// MsgSecretLaneReadRefused, no byte is returned, and the refusal is
// ledgered (reason sensitive_mark_failed) - before the fix the error was
// only logged and the secret-lane bytes were returned anyway.
func TestHandleReadSecretLaneSensitiveMarkFailureRefusesRead(t *testing.T) {
	home := testHome(t)
	rel := "Documents/saglik/tahlil-sonuçları.pdf"
	mustWriteFile(t, filepath.Join(home, filepath.FromSlash(rel)), "gizli tahlil verisi")

	secretLaneGlobs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	marker := &fakeSensitiveMarker{err: errors.New("disk")}
	s := New(home, nil, secretLaneGlobs, filepath.Join(home, "undo"), pc, led, nil, marker)
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc

	out, err := s.HandleReadForTask(context.Background(), "trace-markfail", "task-markfail", FsReadArgs{
		Path: "~/Documents/saglik/tahlil-sonuçları.pdf",
	})
	if err == nil || err.Error() != MsgSecretLaneReadRefused {
		t.Fatalf("HandleReadForTask error = %v, want %q", err, MsgSecretLaneReadRefused)
	}
	if out.ContentBase64 != "" || out.Size != 0 {
		t.Errorf("refused read returned content %q (size %d), want none", out.ContentBase64, out.Size)
	}
	if marker.callCount() != 1 {
		t.Errorf("MarkSensitiveRead calls = %d, want 1", marker.callCount())
	}
	refused := led.find("secret_lane_read_refused")
	if len(refused) != 1 {
		t.Fatalf("secret_lane_read_refused ledger rows = %d, want 1", len(refused))
	}
	if refused[0].payload["reason"] != "sensitive_mark_failed" || refused[0].payload["task_id"] != "task-markfail" {
		t.Errorf("secret_lane_read_refused payload = %v, want reason sensitive_mark_failed, task_id task-markfail", refused[0].payload)
	}
	if n := len(led.find("fs_read")); n != 0 {
		t.Errorf("fs_read ledger rows = %d, want 0 (nothing was read out)", n)
	}
	if calls := esc.snapshot(); len(calls) != 0 {
		t.Errorf("LaneEscalator calls after a failed mark = %+v, want none (the read was already refused)", calls)
	}
}

// ---- FIX-B-6: a secret-lane fs_read escalates the calling task's lane ----

const ibanContent = "IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı"

// TestHandleReadForTaskSecretContentEscalatesLaneBeforeReturningBytes: a
// content hit on an ordinary path escalates the calling task (with the
// classifier's category) before the bytes are returned, and the ledger
// rows carry the task id and category.
func TestHandleReadForTaskSecretContentEscalatesLaneBeforeReturningBytes(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notlar", "siradan.md"), ibanContent)
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	marker := &fakeSensitiveMarker{}
	s := New(home, nil, nil, filepath.Join(home, "undo"), pc, led, nil, marker)
	s.ContentClassifier = fakeContentClassifier{secret: true, category: "finans"}
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc

	out, err := s.HandleReadForTask(context.Background(), "trace-esc", "task-esc", FsReadArgs{Path: "~/notlar/siradan.md"})
	if err != nil {
		t.Fatalf("HandleReadForTask: %v", err)
	}
	if !out.SecretLane || out.ContentBase64 != b64(ibanContent) {
		t.Fatalf("output = %+v, want secret_lane with the file content", out)
	}
	if calls := esc.snapshot(); !reflect.DeepEqual(calls, []escalateCall{{"task-esc", "trace-esc", "finans"}}) {
		t.Fatalf("LaneEscalator calls = %+v, want exactly [{task-esc trace-esc finans}]", calls)
	}
	if marker.callCount() != 1 {
		t.Fatalf("MarkSensitiveRead calls = %d, want 1 (mark, then escalate)", marker.callCount())
	}
	reads := led.find("secret_lane_read")
	if len(reads) != 1 || reads[0].payload["task_id"] != "task-esc" || reads[0].payload["category"] != "finans" {
		t.Fatalf("secret_lane_read rows = %+v, want one carrying task_id task-esc and category finans", reads)
	}
	if rows := led.find("fs_read"); len(rows) != 1 || rows[0].payload["task_id"] != "task-esc" {
		t.Fatalf("fs_read rows = %+v, want one carrying task_id task-esc", rows)
	}
}

// TestHandleReadSecretLaneEscalateFailureRefusesRead: a lane that cannot
// be escalated refuses the read - no byte reaches a task whose lane still
// says normal.
func TestHandleReadSecretLaneEscalateFailureRefusesRead(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notlar", "siradan.md"), ibanContent)
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	led := &fakeLedger{}
	s := New(home, nil, nil, filepath.Join(home, "undo"), pc, led, nil, &fakeSensitiveMarker{})
	s.ContentClassifier = fakeContentClassifier{secret: true, category: "finans"}
	s.LaneEscalator = &fakeLaneEscalator{err: errors.New("db down")}

	out, err := s.HandleReadForTask(context.Background(), "trace-escfail", "task-escfail", FsReadArgs{Path: "~/notlar/siradan.md"})
	if err == nil || err.Error() != MsgSecretLaneReadRefused {
		t.Fatalf("HandleReadForTask error = %v, want %q", err, MsgSecretLaneReadRefused)
	}
	if out.ContentBase64 != "" || out.Size != 0 {
		t.Fatalf("refused read returned content %q (size %d), want none", out.ContentBase64, out.Size)
	}
	refused := led.find("secret_lane_read_refused")
	if len(refused) != 1 || refused[0].payload["reason"] != "lane_escalate_failed" || refused[0].payload["task_id"] != "task-escfail" {
		t.Fatalf("secret_lane_read_refused rows = %+v, want one with reason lane_escalate_failed, task_id task-escfail", refused)
	}
	if n := len(led.find("fs_read")); n != 0 {
		t.Fatalf("fs_read ledger rows = %d, want 0 (nothing was read out)", n)
	}
}

// TestHandleReadPathGlobHitEscalatesWithEmptyCategory: a path-glob hit
// escalates too, with no category (the content classifier is not run).
func TestHandleReadPathGlobHitEscalatesWithEmptyCategory(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "Documents", "saglik", "tahlil.md"), "sonuclar")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	s := New(home, nil, []string{filepath.Join(home, "Documents", "saglik", "**")}, filepath.Join(home, "undo"), pc, &fakeLedger{}, nil, &fakeSensitiveMarker{})
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc

	if _, err := s.HandleReadForTask(context.Background(), "trace-glob", "task-glob", FsReadArgs{Path: "~/Documents/saglik/tahlil.md"}); err != nil {
		t.Fatalf("HandleReadForTask: %v", err)
	}
	if calls := esc.snapshot(); !reflect.DeepEqual(calls, []escalateCall{{"task-glob", "trace-glob", ""}}) {
		t.Fatalf("LaneEscalator calls = %+v, want exactly [{task-glob trace-glob \"\"}]", calls)
	}
}

// TestHandleReadWithoutTaskIDStillCallsEscalator: the task-less HandleRead
// still hands a secret-lane hit to the escalator (with taskID ""), which
// resolves the task from the caller's attribution on kahyad's side - the
// seam is never skipped for want of a declared id.
func TestHandleReadWithoutTaskIDStillCallsEscalator(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notlar", "siradan.md"), ibanContent)
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	s := New(home, nil, nil, filepath.Join(home, "undo"), pc, &fakeLedger{}, nil, &fakeSensitiveMarker{})
	s.ContentClassifier = fakeContentClassifier{secret: true, category: "finans"}
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc

	if _, err := s.HandleRead(context.Background(), "trace-only", FsReadArgs{Path: "~/notlar/siradan.md"}); err != nil {
		t.Fatalf("HandleRead: %v", err)
	}
	if calls := esc.snapshot(); !reflect.DeepEqual(calls, []escalateCall{{"", "trace-only", "finans"}}) {
		t.Fatalf("LaneEscalator calls = %+v, want exactly [{\"\" trace-only finans}]", calls)
	}
}

// TestHandleReadNonSecretNeverEscalates: an ordinary read never touches
// the escalator or the sensitive marker.
func TestHandleReadNonSecretNeverEscalates(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notlar", "a.md"), "yapılacaklar listesi")
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}
	marker := &fakeSensitiveMarker{}
	s := New(home, nil, nil, filepath.Join(home, "undo"), pc, &fakeLedger{}, nil, marker)
	s.ContentClassifier = fakeContentClassifier{secret: false}
	esc := &fakeLaneEscalator{}
	s.LaneEscalator = esc

	if _, err := s.HandleReadForTask(context.Background(), "trace-n", "task-n", FsReadArgs{Path: "~/notlar/a.md"}); err != nil {
		t.Fatalf("HandleReadForTask: %v", err)
	}
	if calls := esc.snapshot(); len(calls) != 0 || marker.callCount() != 0 {
		t.Fatalf("ordinary read: escalator calls %+v, marks %d; want none", calls, marker.callCount())
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

// ---- fs_write: TOCTOU symlinked-ancestor race (BLOCKER regression). ----

// TestHandleWriteTOCTOUSymlinkedAncestorCannotRedirectPastDenyGlob is the
// BLOCKER regression fixture: canonicalize resolves a target whose
// immediate ancestor does NOT yet exist, the deny-glob check runs
// against that (non-denied) canonical path, and then — DURING the
// window Policy.Check/ConsumeToken/checkpointPreImage represent (the
// fake PolicyClient's onCheck hook stands in for that whole wall-clock
// window) — an attacker plants a symlink at the missing ancestor,
// pointing (via a RELATIVE target, so it never "escapes" a broad
// confinement root either) at a deny-glob-PROTECTED directory that is
// itself a SIBLING subtree under the same home directory. Before the
// os.Root confinement fix, atomicWrite's os.MkdirAll/os.CreateTemp/
// os.Rename transparently followed that symlink and the write's bytes
// landed inside the protected directory with HandleWrite returning nil.
// After the fix, HandleWrite must fail closed and NOTHING may land
// inside the protected directory.
func TestHandleWriteTOCTOUSymlinkedAncestorCannotRedirectPastDenyGlob(t *testing.T) {
	home := testHome(t)
	protectedDir := filepath.Join(home, "Library", "LaunchAgents")
	mustMkdirAll(t, protectedDir)
	mustMkdirAll(t, filepath.Join(home, "workspace"))

	// The missing ancestor: ~/workspace/notes does not exist yet at
	// canonicalize time (nor does .../2026 beneath it) — exactly the
	// "a target whose ancestor does not exist at canonicalize time"
	// fixture the finding calls for.
	missingAncestor := filepath.Join(home, "workspace", "notes")
	target := "~/workspace/notes/2026/evil.plist"

	raced := false
	pc := &fakePolicyClient{
		decision: allowDecision("tok-toctou"),
		onCheck: func() {
			raced = true
			// A RELATIVE symlink target: even a confinement scheme that
			// only refuses a symlink "escaping" its own root would let
			// this one through, since ../../Library/LaunchAgents still
			// resolves to somewhere "under home" — this package's own
			// descendConfined must refuse it regardless of where it
			// points, not just when it escapes entirely.
			if err := os.Symlink(filepath.Join("..", "..", "Library", "LaunchAgents"), missingAncestor); err != nil {
				t.Fatalf("os.Symlink (race setup): %v", err)
			}
		},
	}
	led := &fakeLedger{}
	deny := []string{filepath.Join(home, "Library", "LaunchAgents", "**")}
	s := newTestServer(t, home, deny, nil, pc, led)

	_, err := s.HandleWrite(context.Background(), "trace-toctou", "task-1", FsWriteArgs{Path: target, ContentBase64: b64("evil payload")})
	if err == nil {
		t.Fatal("HandleWrite across a raced symlinked ancestor = nil error, want a fail-closed error")
	}
	if !raced {
		t.Fatal("test bug: the race's onCheck hook never ran")
	}

	entries, rerr := os.ReadDir(protectedDir)
	if rerr != nil {
		t.Fatalf("ReadDir(protectedDir): %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("protected dir %s has %d entries after the raced write, want 0 (nothing may land inside a deny-glob-protected directory)", protectedDir, len(entries))
	}
	if got := len(led.find("fs_write")); got != 0 {
		t.Errorf("fs_write ledger events = %d, want 0 (the write must never be reported as having succeeded)", got)
	}
}

// TestHandleWriteTOCTOURacedAbsoluteSymlinkAlsoFailsClosed is a second
// variant of the same race using an ABSOLUTE symlink target (the more
// "obvious" attack os.Root's own built-in escape check already refuses
// on its own) — kept as an explicit regression alongside the relative-
// symlink variant above so a future refactor that accidentally relies
// solely on os.Root's default behavior (which this package's own
// descendConfined intentionally does NOT do — see server.go's "os.Root
// confinement" section doc comment) still has both shapes covered.
func TestHandleWriteTOCTOURacedAbsoluteSymlinkAlsoFailsClosed(t *testing.T) {
	home := testHome(t)
	protectedDir := filepath.Join(home, "Library", "LaunchAgents")
	mustMkdirAll(t, protectedDir)
	mustMkdirAll(t, filepath.Join(home, "workspace"))

	missingAncestor := filepath.Join(home, "workspace", "notes")
	target := "~/workspace/notes/evil.plist"

	pc := &fakePolicyClient{
		decision: allowDecision("tok-toctou-abs"),
		onCheck: func() {
			if err := os.Symlink(protectedDir, missingAncestor); err != nil {
				t.Fatalf("os.Symlink (race setup): %v", err)
			}
		},
	}
	deny := []string{filepath.Join(home, "Library", "LaunchAgents", "**")}
	s := newTestServer(t, home, deny, nil, pc, &fakeLedger{})

	_, err := s.HandleWrite(context.Background(), "trace-toctou-abs", "task-1", FsWriteArgs{Path: target, ContentBase64: b64("evil payload")})
	if err == nil {
		t.Fatal("HandleWrite across a raced absolute-symlinked ancestor = nil error, want a fail-closed error")
	}

	entries, rerr := os.ReadDir(protectedDir)
	if rerr != nil {
		t.Fatalf("ReadDir(protectedDir): %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("protected dir %s has %d entries after the raced write, want 0", protectedDir, len(entries))
	}
}

// TestHandleWriteCreatesNestedNotYetExistingDirectories is a non-attack
// sanity companion to the TOCTOU regression tests above: a brand-new,
// MULTI-LEVEL nested directory chain (no race, no symlink involved) must
// still be created correctly through the os.Root-confined write path —
// proving the BLOCKER fix's descendConfined walk didn't regress the
// legitimate "write creates its own parent directory tree" capability
// atomicWrite's original doc comment described.
func TestHandleWriteCreatesNestedNotYetExistingDirectories(t *testing.T) {
	home := testHome(t)
	pc := &fakePolicyClient{decision: allowDecision("tok-nested")}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	target := filepath.Join(home, "Documents", "notes", "2026", "07", "journal.md")
	out, err := s.HandleWrite(context.Background(), "trace-nested", "task-1", FsWriteArgs{
		Path: "~/Documents/notes/2026/07/journal.md", ContentBase64: b64("merhaba"),
	})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if out.PreExisted {
		t.Fatal("PreExisted = true, want false")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "merhaba" {
		t.Fatalf("nested target content = %q (err=%v), want %q", content, err, "merhaba")
	}
}

// TestTaskDirNameRejectsTraversal guards defense-in-depth: a task_id with a
// path separator or ".." must not be used verbatim as an undo subdir name
// (path-traversal escape from UndoDir).
func TestTaskDirNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../evil", "a/b", "a\\b", "x..y", "..", "t_../.."} {
		if got := taskDirName(bad); got != "_unscoped" {
			t.Errorf("taskDirName(%q) = %q, want _unscoped", bad, got)
		}
	}
	if got := taskDirName("t_abc123"); got != "t_abc123" {
		t.Errorf("taskDirName(valid) = %q, want t_abc123", got)
	}
}
