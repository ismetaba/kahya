// undo_per_op_test.go covers FIX-B-8: one undo record per executed
// fs_write/fs_delete, keyed by the op id kahyad returns from ConsumeToken
// (the consumed token's token_hash), never by trace_id. A task (one trace)
// that writes N times keeps N independently undoable records; undoing them
// newest first (the order kahyad's TriggerUndo hands back) restores the
// original bytes, and no pre-image copy is left behind under UndoDir.
package fs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func undoTaskEntries(t *testing.T, home, taskID string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "undo", taskID))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read undo dir: %v", err)
	}
	return entries
}

func mustReadString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// writeOp runs one fs_write with a fresh one-time token and returns its op id.
func writeOp(t *testing.T, s *Server, pc *fakePolicyClient, trace, task, path, content, token string) string {
	t.Helper()
	pc.decision = allowDecision(token)
	out, err := s.HandleWrite(context.Background(), trace, task, FsWriteArgs{Path: path, ContentBase64: b64(content)})
	if err != nil {
		t.Fatalf("HandleWrite %s: %v", path, err)
	}
	if out.OpID == "" {
		t.Fatalf("HandleWrite %s returned an empty OpID", path)
	}
	return out.OpID
}

// TestUndoTwoWritesSameTraceRestoresBothLIFO is the handbook's B-8
// reproduction (a) under the per-op API: two writes in one trace, both
// undone newest first, both files byte-exact again, no orphan copy.
func TestUndoTwoWritesSameTraceRestoresBothLIFO(t *testing.T) {
	home := testHome(t)
	a, b := filepath.Join(home, "a.txt"), filepath.Join(home, "b.txt")
	mustWriteFile(t, a, "A-orig")
	mustWriteFile(t, b, "B-orig")
	pc := &fakePolicyClient{}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)
	ctx := context.Background()

	opA := writeOp(t, s, pc, "trace-1", "task-1", "~/a.txt", "A-new", "tok-a")
	opB := writeOp(t, s, pc, "trace-1", "task-1", "~/b.txt", "B-new", "tok-b")
	if opA == opB {
		t.Fatalf("two operations share op id %q", opA)
	}
	if n := s.registry.countForTrace("trace-1"); n != 2 {
		t.Fatalf("registry holds %d records for trace-1, want 2 (one per operation)", n)
	}

	for _, op := range []string{opB, opA} { // LIFO, exactly TriggerUndo's order
		res, err := s.UndoOp(ctx, "trace-1", op)
		if err != nil {
			t.Fatalf("UndoOp(%s): %v", op, err)
		}
		if res.OpID != op || res.Tool != "fs_write" {
			t.Fatalf("UndoOp(%s) = %+v", op, res)
		}
	}
	if got := mustReadString(t, a); got != "A-orig" {
		t.Errorf("a.txt = %q after undo, want A-orig", got)
	}
	if got := mustReadString(t, b); got != "B-orig" {
		t.Errorf("b.txt = %q after undo, want B-orig", got)
	}
	if e := undoTaskEntries(t, home, "task-1"); len(e) != 0 {
		t.Errorf("undo/task-1 holds %d orphan pre-image copies after undoing every op, want 0", len(e))
	}
	if n := s.registry.countForTrace("trace-1"); n != 0 {
		t.Errorf("registry still holds %d records for trace-1, want 0", n)
	}
	executed := led.find("undo_executed")
	if len(executed) != 2 {
		t.Fatalf("undo_executed ledger events = %d, want 2", len(executed))
	}
	for i, want := range []string{opB, opA} {
		if got := executed[i].payload["op_id"]; got != want {
			t.Errorf("undo_executed[%d].op_id = %v, want %s", i, got, want)
		}
	}
	for _, e := range led.find("fs_write") {
		if id, _ := e.payload["op_id"].(string); id != opA && id != opB {
			t.Errorf("fs_write ledger op_id = %v, want one of the two op ids", e.payload["op_id"])
		}
	}
}

// TestUndoSameFileTwiceLIFORestoresOriginal: two writes to the SAME file
// keep two separate pre-image copies (named by op id); undoing newest first
// steps the file back v2 -> v1 -> original.
func TestUndoSameFileTwiceLIFORestoresOriginal(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "A-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()

	op1 := writeOp(t, s, pc, "trace-2", "task-2", "~/a.txt", "A-v1", "tok-1")
	op2 := writeOp(t, s, pc, "trace-2", "task-2", "~/a.txt", "A-v2", "tok-2")
	if e := undoTaskEntries(t, home, "task-2"); len(e) != 2 {
		t.Fatalf("undo/task-2 holds %d pre-image copies, want 2 (one per operation)", len(e))
	}

	if _, err := s.UndoOp(ctx, "trace-2", op2); err != nil {
		t.Fatalf("UndoOp(newest): %v", err)
	}
	if got := mustReadString(t, a); got != "A-v1" {
		t.Fatalf("after undoing the newest write a.txt = %q, want A-v1", got)
	}
	if _, err := s.UndoOp(ctx, "trace-2", op1); err != nil {
		t.Fatalf("UndoOp(oldest): %v", err)
	}
	if got := mustReadString(t, a); got != "A-orig" {
		t.Fatalf("after undoing both writes a.txt = %q, want A-orig", got)
	}
	if e := undoTaskEntries(t, home, "task-2"); len(e) != 0 {
		t.Errorf("undo/task-2 holds %d copies after both undos, want 0", len(e))
	}
}

// TestUndoSameFileTwiceGitCheckpointLIFO is the git-checkpoint twin: both
// writes checkpoint a blob, LIFO undo restores the original bytes.
func TestUndoSameFileTwiceGitCheckpointLIFO(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := testHome(t)
	mustGitInit(t, home)
	a := filepath.Join(home, "tracked.txt")
	mustWriteFile(t, a, "orig\n")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()

	var ops []string
	for i, content := range []string{"v1\n", "v2\n"} {
		pc.decision = allowDecision("tok-git-" + content[:2])
		out, err := s.HandleWrite(ctx, "trace-g", "task-g", FsWriteArgs{Path: "~/tracked.txt", ContentBase64: b64(content)})
		if err != nil {
			t.Fatalf("HandleWrite #%d: %v", i, err)
		}
		if !out.GitCheckpoint {
			t.Fatalf("HandleWrite #%d: GitCheckpoint = false, want true", i)
		}
		ops = append(ops, out.OpID)
	}
	if _, err := s.UndoOp(ctx, "trace-g", ops[1]); err != nil {
		t.Fatalf("UndoOp(newest): %v", err)
	}
	if got := mustReadString(t, a); got != "v1\n" {
		t.Fatalf("after undoing the newest write = %q, want v1", got)
	}
	if _, err := s.UndoOp(ctx, "trace-g", ops[0]); err != nil {
		t.Fatalf("UndoOp(oldest): %v", err)
	}
	if got := mustReadString(t, a); got != "orig\n" {
		t.Fatalf("after undoing both writes = %q, want orig", got)
	}
}

// TestUndoWriteThenDeleteSameTraceKeepsBothRecords: before B-8 the delete's
// registry Put overwrote the write's record (same trace key).
func TestUndoWriteThenDeleteSameTraceKeepsBothRecords(t *testing.T) {
	home := testHome(t)
	a, b := filepath.Join(home, "a.txt"), filepath.Join(home, "b.txt")
	mustWriteFile(t, a, "A-orig")
	mustWriteFile(t, b, "B-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()

	opWrite := writeOp(t, s, pc, "trace-wd", "task-wd", "~/a.txt", "A-new", "tok-w")
	pc.decision = allowDecision("tok-d")
	del, err := s.HandleDelete(ctx, "trace-wd", "task-wd", FsDeleteArgs{Path: "~/b.txt"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if del.OpID == "" || del.OpID == opWrite {
		t.Fatalf("delete op id = %q (write op %q), want a distinct non-empty id", del.OpID, opWrite)
	}
	if n := s.registry.countForTrace("trace-wd"); n != 2 {
		t.Fatalf("registry holds %d records for the trace, want 2", n)
	}
	res, err := s.UndoOp(ctx, "trace-wd", del.OpID)
	if err != nil || res.Tool != "fs_delete" {
		t.Fatalf("UndoOp(delete) = %+v, %v", res, err)
	}
	if got := mustReadString(t, b); got != "B-orig" {
		t.Fatalf("b.txt after undoing the delete = %q, want B-orig", got)
	}
	if _, err := s.UndoOp(ctx, "trace-wd", opWrite); err != nil {
		t.Fatalf("UndoOp(write): %v", err)
	}
	if got := mustReadString(t, a); got != "A-orig" {
		t.Fatalf("a.txt after undoing the write = %q, want A-orig", got)
	}
}

// TestUndoOpRejectsTraceMismatch: a record is never executed for a trace
// it does not belong to (fail-closed), and stays available for its own.
func TestUndoOpRejectsTraceMismatch(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "A-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()

	op := writeOp(t, s, pc, "trace-1", "task-1", "~/a.txt", "A-new", "tok-1")
	if _, err := s.UndoOp(ctx, "trace-2", op); !errors.Is(err, ErrNoUndoRecord) {
		t.Fatalf("UndoOp(other trace) err = %v, want ErrNoUndoRecord", err)
	}
	if got := mustReadString(t, a); got != "A-new" {
		t.Fatalf("a.txt = %q after a refused cross-trace undo, want A-new (untouched)", got)
	}
	if _, err := s.UndoOp(ctx, "trace-1", op); err != nil {
		t.Fatalf("UndoOp(own trace) after the refused attempt: %v", err)
	}
	if got := mustReadString(t, a); got != "A-orig" {
		t.Fatalf("a.txt = %q, want A-orig", got)
	}
}

// TestPurgeExpiredRemovesOnlyThatOpCopy: the expiry hook purges exactly
// the expired operation's copy and record; a sibling op of the same trace
// stays undoable.
func TestPurgeExpiredRemovesOnlyThatOpCopy(t *testing.T) {
	home := testHome(t)
	a, b := filepath.Join(home, "a.txt"), filepath.Join(home, "b.txt")
	mustWriteFile(t, a, "A-orig")
	mustWriteFile(t, b, "B-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()

	opA := writeOp(t, s, pc, "trace-1", "task-1", "~/a.txt", "A-new", "tok-a")
	opB := writeOp(t, s, pc, "trace-1", "task-1", "~/b.txt", "B-new", "tok-b")
	s.PurgeExpired("trace-1", "task-1", "fs_write", opA)

	entries := undoTaskEntries(t, home, "task-1")
	if len(entries) != 1 || entries[0].Name() != opB {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("undo/task-1 after purging op A = %v, want exactly [%s]", names, opB)
	}
	if _, err := s.UndoOp(ctx, "trace-1", opA); !errors.Is(err, ErrNoUndoRecord) {
		t.Fatalf("UndoOp(purged op) err = %v, want ErrNoUndoRecord", err)
	}
	if _, err := s.UndoOp(ctx, "trace-1", opB); err != nil {
		t.Fatalf("UndoOp(surviving op): %v", err)
	}
	if got := mustReadString(t, b); got != "B-orig" {
		t.Fatalf("b.txt = %q, want B-orig", got)
	}
}

// TestFailedWriteLeavesNoOrphanCopy: the pre-image copy is made before the
// write; when the write itself fails the copy is removed and no record is
// registered (nothing to undo, nothing for a purge to miss).
func TestFailedWriteLeavesNoOrphanCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	home := testHome(t)
	ro := filepath.Join(home, "ro")
	mustWriteFile(t, filepath.Join(ro, "a.txt"), "A-orig")
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })

	pc := &fakePolicyClient{decision: allowDecision("tok-ro")}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	if _, err := s.HandleWrite(context.Background(), "trace-ro", "task-ro", FsWriteArgs{Path: "~/ro/a.txt", ContentBase64: b64("A-new")}); err == nil {
		t.Fatal("HandleWrite into a read-only directory succeeded, want an error")
	}
	if pc.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1 (the write failed after the token was consumed)", pc.consumeCalls)
	}
	if e := undoTaskEntries(t, home, "task-ro"); len(e) != 0 {
		t.Fatalf("undo/task-ro holds %d pre-image copies after a failed write, want 0", len(e))
	}
	if n := s.registry.countForTrace("trace-ro"); n != 0 {
		t.Fatalf("registry holds %d records after a failed write, want 0", n)
	}
}

func TestNormalizeOpID(t *testing.T) {
	keep := strings.Repeat("ab", 32)
	if got := normalizeOpID(keep); got != keep {
		t.Fatalf("normalizeOpID(64 hex) = %q, want it verbatim", got)
	}
	for _, in := range []string{"", "not-hex!", "abc", strings.Repeat("a", 300), "ABCDEF0123456789ABCDEF", "../../etc/passwd0000"} {
		got := normalizeOpID(in)
		if got == in {
			t.Errorf("normalizeOpID(%q) returned the input verbatim, want a fresh local id", in)
		}
		if len(got) != 32 || strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("normalizeOpID(%q) = %q, want 32 lowercase hex", in, got)
		}
	}
	if normalizeOpID("") == normalizeOpID("") {
		t.Error("two locally minted op ids collided")
	}
}
