package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeReceipts mimics kahyad's task.Receipts closely enough for this
// package (FIX-I-4, per worker run since FIX-FINAL receipt-replay-per-run):
// a receipt is stored per (task, tool, args hash) under the current worker
// run once an effect succeeds; Replay - and Execute - answer only a receipt
// of an EARLIER run that the current run has not consumed yet (each at most
// once per run); resume() starts the next run. persistErr simulates "the
// effect ran, the receipt could not be written".
type fakeReceipts struct {
	mu           sync.Mutex
	run          int // current worker run; 0 means run 1
	stored       map[string][]*fakeReceipt
	replayCalls  int
	executeCalls int
	effectRuns   int
	executed     []ReceiptInput
	replayErr    error
	persistErr   error
	// effectCtxCancelable records whether the context handed to the effect
	// can be canceled (it must not: the effect is detached).
	effectCtxCancelable bool
}

// fakeReceipt is one stored receipt and the runs that wrote/consumed it.
type fakeReceipt struct {
	raw              json.RawMessage
	run, replayedRun int
}

func receiptKey(in ReceiptInput) string { return in.TaskID + "|" + in.Tool + "|" + in.ArgsHash }

// resume starts the task's next worker run (the stand-in for kahyad's
// tasks.worker_run bump at a redispatch).
func (f *fakeReceipts) resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = f.currentRunLocked() + 1
}

func (f *fakeReceipts) currentRunLocked() int {
	if f.run == 0 {
		return 1
	}
	return f.run
}

// replayableLocked consumes the oldest earlier-run receipt of in's key the
// current run has not replayed yet.
func (f *fakeReceipts) replayableLocked(in ReceiptInput) (json.RawMessage, bool) {
	run := f.currentRunLocked()
	for _, r := range f.stored[receiptKey(in)] {
		if r.run < run && r.replayedRun < run {
			r.replayedRun = run
			return r.raw, true
		}
	}
	return nil, false
}

func (f *fakeReceipts) Replay(_ context.Context, in ReceiptInput) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replayCalls++
	if f.replayErr != nil {
		return nil, false, f.replayErr
	}
	raw, ok := f.replayableLocked(in)
	return raw, ok, nil
}

func (f *fakeReceipts) Execute(ctx context.Context, in ReceiptInput, effect func(context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	f.mu.Lock()
	f.executeCalls++
	f.executed = append(f.executed, in)
	if raw, ok := f.replayableLocked(in); ok {
		f.mu.Unlock()
		return raw, true, nil
	}
	f.effectCtxCancelable = ctx.Done() != nil
	f.mu.Unlock()

	raw, err := effect(ctx)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.effectRuns++
	if err != nil {
		return nil, false, err
	}
	if f.persistErr != nil {
		return raw, false, f.persistErr
	}
	if f.stored == nil {
		f.stored = make(map[string][]*fakeReceipt)
	}
	f.stored[receiptKey(in)] = append(f.stored[receiptKey(in)], &fakeReceipt{raw: raw, run: f.currentRunLocked()})
	return raw, false, nil
}

// rotatingTokens makes every Check mint a fresh token, so a test can tell a
// real second execution (it would succeed) from a replay.
func rotatingTokens(pc *fakePolicyClient) {
	n := 0
	pc.onCheck = func() {
		n++
		pc.decision = allowDecision(fmt.Sprintf("tok-%d", n))
	}
}

func newReceiptServer(t *testing.T) (*Server, string, *fakePolicyClient, *fakeLedger, *fakeReceipts) {
	t.Helper()
	home := testHome(t)
	pc := &fakePolicyClient{}
	rotatingTokens(pc)
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)
	rex := &fakeReceipts{}
	s.Receipts = rex
	return s, home, pc, led, rex
}

// TestHandleWriteReplaysReceiptWithoutSecondWrite is FIX-I-4's core fs test:
// a resumed session (the task's next worker run) re-issuing the SAME
// fs_write of the SAME task gets the stored result - no second write, no
// second pre-image checkpoint, no second policy decision (so no second
// approval and no token burnt) - marked replayed:true with the model-facing
// MsgReplayedFromEarlierRun. A further identical write of the resumed run
// executes (FIX-FINAL receipt-replay-per-run: each earlier receipt is
// replayed at most once per run).
func TestHandleWriteReplaysReceiptWithoutSecondWrite(t *testing.T) {
	s, home, pc, led, rex := newReceiptServer(t)
	target := filepath.Join(home, "notlar.md")
	mustWriteFile(t, target, "eski\n")
	args := FsWriteArgs{Path: "~/notlar.md", ContentBase64: b64("eski\nmerhaba\n")}

	out1, err := s.HandleWrite(context.Background(), "trace-1", "task-1", args)
	if err != nil {
		t.Fatalf("first HandleWrite: %v", err)
	}
	if out1.Replayed {
		t.Fatal("first write reported Replayed")
	}
	// The user (or anything) edits the file afterwards: a replay must not
	// rewrite it.
	mustWriteFile(t, target, "kullanici duzenledi\n")

	rex.resume()
	out2, err := s.HandleWrite(context.Background(), "trace-1", "task-1", args)
	if err != nil {
		t.Fatalf("second (resumed) HandleWrite: %v", err)
	}
	if !out2.Replayed {
		t.Fatal("second identical write was not answered from its receipt")
	}
	if out2.ReplayNote != MsgReplayedFromEarlierRun {
		t.Fatalf("replay note = %q, want %q", out2.ReplayNote, MsgReplayedFromEarlierRun)
	}
	out2.Replayed, out2.ReplayNote = false, ""
	if out2 != out1 {
		t.Fatalf("replayed result = %+v, want the stored %+v", out2, out1)
	}
	if b, _ := os.ReadFile(target); string(b) != "kullanici duzenledi\n" {
		t.Fatalf("file content after the replay = %q: the write ran a second time", b)
	}
	if got := len(led.find("fs_write")); got != 1 {
		t.Fatalf("fs_write executions = %d, want 1 (checkpoint + write ran once)", got)
	}
	if got := len(led.find("fs_write_replayed")); got != 1 {
		t.Fatalf("fs_write_replayed = %d, want 1", got)
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("Policy.Check/ConsumeToken calls = %d/%d, want 1/1 (a replay takes no policy decision)", pc.checkCalls, pc.consumeCalls)
	}
	if rex.effectRuns != 1 {
		t.Fatalf("effect runs = %d, want 1", rex.effectRuns)
	}
	// The receipt key is the canonical {path, content} the token binds to;
	// class and token come from the decision.
	in := rex.executed[0]
	cp, _ := Canonicalize(home, args.Path)
	if want := ReceiptArgsHash(buildToolInput(cp.Match, []byte("eski\nmerhaba\n"))); in.ArgsHash != want {
		t.Fatalf("receipt args hash = %s, want sha256 of the canonical tool input %s", in.ArgsHash, want)
	}
	if in.Tool != "fs_write" || in.Class != "W1" || in.ApprovalToken != "tok-1" || in.TaskID != "task-1" || in.TraceID != "trace-1" {
		t.Fatalf("receipt input = %+v", in)
	}
	if rex.effectCtxCancelable {
		t.Fatal("the effect ran on a cancelable context: a dying worker/bridge would abandon a half-done write")
	}

	// The resumed run's NEXT identical write is a live repeat: it executes.
	out3, err := s.HandleWrite(context.Background(), "trace-1", "task-1", args)
	if err != nil || out3.Replayed || out3.ReplayNote != "" {
		t.Fatalf("third identical write = (%+v, %v), want a fresh execution", out3, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "eski\nmerhaba\n" {
		t.Fatalf("file after the live repeat = %q, want the written content", b)
	}
	if pc.checkCalls != 2 || len(led.find("fs_write")) != 2 {
		t.Fatalf("checks/executions after the live repeat = %d/%d, want 2/2", pc.checkCalls, len(led.find("fs_write")))
	}
}

// TestHandleWriteLiveRepeatExecutesWithoutResume: inside one worker run an
// identical fs_write is a legitimate repeat (FIX-FINAL
// receipt-replay-per-run) - X, Y, X leaves X on disk.
func TestHandleWriteLiveRepeatExecutesWithoutResume(t *testing.T) {
	s, home, pc, led, _ := newReceiptServer(t)
	for _, content := range []string{"X", "Y", "X"} {
		out, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/xyx.txt", ContentBase64: b64(content)})
		if err != nil || out.Replayed {
			t.Fatalf("HandleWrite(%s) = (%+v, %v), want an execution", content, out, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "xyx.txt")); string(b) != "X" {
		t.Fatalf("content = %q, want X", b)
	}
	if got := len(led.find("fs_write")); got != 3 || pc.checkCalls != 3 {
		t.Fatalf("executions/checks = %d/%d, want 3/3", got, pc.checkCalls)
	}
}

func TestHandleWriteDifferentContentIsNotReplayed(t *testing.T) {
	s, home, pc, led, _ := newReceiptServer(t)
	for _, content := range []string{"v1", "v2"} {
		if _, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/x.txt", ContentBase64: b64(content)}); err != nil {
			t.Fatalf("HandleWrite(%s): %v", content, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "x.txt")); string(b) != "v2" {
		t.Fatalf("content = %q, want v2 (different bytes are a different call)", b)
	}
	if got := len(led.find("fs_write")); got != 2 || pc.checkCalls != 2 {
		t.Fatalf("fs_write executions/checks = %d/%d, want 2/2", got, pc.checkCalls)
	}
}

// TestHandleWriteWithoutReceiptsRunsDirectly: no executor wired, or no task
// id to key on, keeps the direct pre-FIX-I-4 path (every identical call runs).
func TestHandleWriteWithoutReceiptsRunsDirectly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		taskID string
		wire   bool
	}{{"nil executor", "task-1", false}, {"no task id", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, pc, led, rex := newReceiptServer(t)
			if !tc.wire {
				s.Receipts = nil
			}
			args := FsWriteArgs{Path: "~/direct.txt", ContentBase64: b64("same")}
			for i := 0; i < 2; i++ {
				if _, err := s.HandleWrite(context.Background(), "trace-1", tc.taskID, args); err != nil {
					t.Fatalf("HandleWrite #%d: %v", i, err)
				}
			}
			if got := len(led.find("fs_write")); got != 2 || pc.checkCalls != 2 {
				t.Fatalf("executions/checks = %d/%d, want 2/2 on the direct path", got, pc.checkCalls)
			}
			if rex.replayCalls != 0 || rex.executeCalls != 0 {
				t.Fatalf("receipt executor used (%d replays, %d executes) without a task id or wiring", rex.replayCalls, rex.executeCalls)
			}
		})
	}
}

// TestHandleDeleteReplaysReceiptWithoutSecondMove: the resumed session's
// (next worker run's) second fs_delete finds the file already gone - it is
// answered from the receipt (same Trash path), not failed and not moved
// again.
func TestHandleDeleteReplaysReceiptWithoutSecondMove(t *testing.T) {
	s, home, pc, led, rex := newReceiptServer(t)
	s.TrashDir = filepath.Join(home, ".Trash")
	mustWriteFile(t, filepath.Join(home, "sil.txt"), "bye")
	args := FsDeleteArgs{Path: "~/sil.txt"}

	out1, err := s.HandleDelete(context.Background(), "trace-1", "task-1", args)
	if err != nil {
		t.Fatalf("first HandleDelete: %v", err)
	}
	rex.resume()
	out2, err := s.HandleDelete(context.Background(), "trace-1", "task-1", args)
	if err != nil {
		t.Fatalf("second (resumed) HandleDelete: %v - a completed delete must replay, not fail", err)
	}
	if !out2.Replayed || out2.ReplayNote != MsgReplayedFromEarlierRun || out2.TrashPath != out1.TrashPath || out2.Path != out1.Path {
		t.Fatalf("replayed delete = %+v, want the stored %+v marked Replayed", out2, out1)
	}
	if got := len(led.find("fs_delete")); got != 1 {
		t.Fatalf("fs_delete executions = %d, want 1", got)
	}
	if got := len(led.find("fs_delete_replayed")); got != 1 {
		t.Fatalf("fs_delete_replayed = %d, want 1", got)
	}
	if pc.checkCalls != 1 {
		t.Fatalf("Policy.Check calls = %d, want 1", pc.checkCalls)
	}
	entries, _ := os.ReadDir(s.TrashDir)
	if len(entries) != 1 {
		t.Fatalf("Trash entries = %d, want 1", len(entries))
	}
}

// TestHandleWriteReplayLookupErrorFailsClosed: a receipt lookup that cannot
// be answered refuses the call - nothing is checked, consumed or written.
func TestHandleWriteReplayLookupErrorFailsClosed(t *testing.T) {
	s, home, pc, led, rex := newReceiptServer(t)
	rex.replayErr = errors.New("database is locked")
	if _, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/y.txt", ContentBase64: b64("x")}); err == nil {
		t.Fatal("HandleWrite with a failing receipt lookup = nil error, want a refusal")
	}
	if pc.checkCalls != 0 || len(led.find("fs_write")) != 0 {
		t.Fatalf("checks=%d writes=%d after a failed lookup, want 0/0", pc.checkCalls, len(led.find("fs_write")))
	}
	if _, err := os.Stat(filepath.Join(home, "y.txt")); !os.IsNotExist(err) {
		t.Fatalf("target exists after a refused call (stat err=%v)", err)
	}
}

// TestHandleWriteReceiptPersistFailureReportsError: the write happened but
// its receipt could not be persisted - the caller is told (fail-closed),
// and the executor's error is what it sees.
func TestHandleWriteReceiptPersistFailureReportsError(t *testing.T) {
	s, home, _, led, rex := newReceiptServer(t)
	rex.persistErr = errors.New("task: effect ran but its receipt could not be persisted")
	_, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/z.txt", ContentBase64: b64("z")})
	if err == nil || err.Error() != rex.persistErr.Error() {
		t.Fatalf("HandleWrite error = %v, want the receipt persistence error", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "z.txt")); string(b) != "z" {
		t.Fatalf("content = %q: the effect itself ran and stays (no rollback is claimed)", b)
	}
	if got := len(led.find("fs_write")); got != 1 {
		t.Fatalf("fs_write executions = %d, want 1", got)
	}
}

// TestHandleWriteDenyGlobHitNeverConsultsReceipts: the mechanical deny-glob
// check stays first - a denied path never even reaches the receipt lookup.
func TestHandleWriteDenyGlobHitNeverConsultsReceipts(t *testing.T) {
	s, home, _, _, rex := newReceiptServer(t)
	s.DenyGlobs = []string{filepath.Join(home, ".zshrc")}
	if _, err := s.HandleWrite(context.Background(), "trace-1", "task-1", FsWriteArgs{Path: "~/.zshrc", ContentBase64: b64("x")}); err == nil {
		t.Fatal("deny-glob hit = nil error")
	}
	if rex.replayCalls != 0 || rex.executeCalls != 0 {
		t.Fatalf("receipt executor consulted (%d/%d) for a deny-glob hit", rex.replayCalls, rex.executeCalls)
	}
}
