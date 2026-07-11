// undo_test.go covers this package's undo recipes (undo.go): fs_write's
// pre-op-checkpoint restore (both the git-checkpoint and non-git fallback-
// copy variants), fs_delete's Trash restore, and pre-image purge on undo-
// window expiry.
package fs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ---- fs_write: checkpoint + undo ----

func TestHandleWriteGitCheckpointThenUndoByteIdenticalRestore(t *testing.T) {
	home := testHome(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	mustGitInit(t, home)
	target := filepath.Join(home, "tracked.txt")
	const original = "pre-image content\n"
	mustWriteFile(t, target, original)

	pc := &fakePolicyClient{decision: allowDecision("tok-git")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	out, err := s.HandleWrite(context.Background(), "trace-git", "task-1", FsWriteArgs{Path: "~/tracked.txt", ContentBase64: b64("new content\n")})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if !out.GitCheckpoint {
		t.Fatal("GitCheckpoint = false, want true (target is inside a git work tree)")
	}
	if !out.PreExisted {
		t.Error("PreExisted = false, want true")
	}

	if err := s.UndoWrite(context.Background(), "trace-git"); err != nil {
		t.Fatalf("UndoWrite: %v", err)
	}
	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile after undo: %v", err)
	}
	if string(restored) != original {
		t.Errorf("restored content = %q, want byte-identical %q", restored, original)
	}
	if got := len(led.find("undo_executed")); got != 1 {
		t.Errorf("undo_executed ledger events = %d, want 1", got)
	}
}

func TestHandleWriteNonGitFallbackCopyThenUndoRestore(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "plain.txt")
	const original = "plain original\n"
	mustWriteFile(t, target, original)

	pc := &fakePolicyClient{decision: allowDecision("tok-copy")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	out, err := s.HandleWrite(context.Background(), "trace-copy", "task-42", FsWriteArgs{Path: "~/plain.txt", ContentBase64: b64("overwritten\n")})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if out.GitCheckpoint {
		t.Fatal("GitCheckpoint = true, want false (not inside a git work tree)")
	}

	copyDir := filepath.Join(home, "undo", "task-42")
	entries, err := os.ReadDir(copyDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("undo copy dir %s: entries=%v err=%v, want exactly 1 file", copyDir, entries, err)
	}

	if err := s.UndoWrite(context.Background(), "trace-copy"); err != nil {
		t.Fatalf("UndoWrite: %v", err)
	}
	restored, err := os.ReadFile(target)
	if err != nil || string(restored) != original {
		t.Fatalf("restored content = %q (err=%v), want %q", restored, err, original)
	}

	entries, _ = os.ReadDir(copyDir)
	if len(entries) != 0 {
		t.Errorf("undo copy dir still has %d entries after a successful undo, want 0 (cleaned up)", len(entries))
	}
}

func TestHandleWriteNewFileUndoMovesToTrash(t *testing.T) {
	home := testHome(t)
	pc := &fakePolicyClient{decision: allowDecision("tok-new")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	target := filepath.Join(home, "brand-new.txt")
	out, err := s.HandleWrite(context.Background(), "trace-new", "task-1", FsWriteArgs{Path: "~/brand-new.txt", ContentBase64: b64("hello")})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if out.PreExisted {
		t.Fatal("PreExisted = true, want false (file did not exist before)")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("newly-written file missing: %v", err)
	}

	if err := s.UndoWrite(context.Background(), "trace-new"); err != nil {
		t.Fatalf("UndoWrite: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file still present at original location after undoing a brand-new write: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".Trash", "brand-new.txt")); err != nil {
		t.Errorf("undone new-file write not found in Trash: %v", err)
	}
}

// ---- fs_delete + undo_delete ----

func TestHandleDeleteMovesToTrashNeverUnlink(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "todelete.txt")
	mustWriteFile(t, target, "bye")
	pc := &fakePolicyClient{decision: allowDecision("tok-del")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	out, err := s.HandleDelete(context.Background(), "trace-del", "task-1", FsDeleteArgs{Path: "~/todelete.txt"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("original file still present after delete: err=%v", err)
	}
	if _, err := os.Stat(out.TrashPath); err != nil {
		t.Errorf("Trash entry %s missing: %v", out.TrashPath, err)
	}
	if got := len(led.find("fs_delete")); got != 1 {
		t.Errorf("fs_delete ledger events = %d, want 1", got)
	}
}

func TestHandleDeleteCollisionSafeSuffix(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "a", "dup.txt"), "first")
	mustWriteFile(t, filepath.Join(home, "b", "dup.txt"), "second")
	pc := &fakePolicyClient{decision: allowDecision("tok-1")}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	out1, err := s.HandleDelete(context.Background(), "trace-a", "task-1", FsDeleteArgs{Path: "~/a/dup.txt"})
	if err != nil {
		t.Fatalf("HandleDelete a: %v", err)
	}
	pc.decision = allowDecision("tok-2") // a fresh one-time token for the second delete
	out2, err := s.HandleDelete(context.Background(), "trace-b", "task-1", FsDeleteArgs{Path: "~/b/dup.txt"})
	if err != nil {
		t.Fatalf("HandleDelete b: %v", err)
	}
	if out1.TrashPath == out2.TrashPath {
		t.Fatalf("both deletes landed at the same Trash path %q, want a collision-safe suffix", out1.TrashPath)
	}
}

func TestUndoDeleteRestoresOriginalLocation(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "restoreme.txt")
	mustWriteFile(t, target, "keep me")
	pc := &fakePolicyClient{decision: allowDecision("tok-restore")}
	led := &fakeLedger{}
	s := newTestServer(t, home, nil, nil, pc, led)

	if _, err := s.HandleDelete(context.Background(), "trace-restore", "task-1", FsDeleteArgs{Path: "~/restoreme.txt"}); err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	if err := s.UndoDelete(context.Background(), "trace-restore"); err != nil {
		t.Fatalf("UndoDelete: %v", err)
	}
	restored, err := os.ReadFile(target)
	if err != nil || string(restored) != "keep me" {
		t.Fatalf("restored content = %q (err=%v), want %q", restored, err, "keep me")
	}
	if got := len(led.find("undo_executed")); got != 1 {
		t.Errorf("undo_executed ledger events = %d, want 1", got)
	}
}

// ---- purge on expiry ----

func TestPurgeExpiredRemovesFallbackCopyAndEmptiesUndoDir(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "toexpire.txt")
	mustWriteFile(t, target, "pre-image")
	pc := &fakePolicyClient{decision: allowDecision("tok-expire")}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})

	if _, err := s.HandleWrite(context.Background(), "trace-expire", "task-99", FsWriteArgs{Path: "~/toexpire.txt", ContentBase64: b64("post-image")}); err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}

	taskDir := filepath.Join(home, "undo", "task-99")
	entries, err := os.ReadDir(taskDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("undo dir before purge: entries=%v err=%v, want exactly 1", entries, err)
	}

	s.PurgeExpired("trace-expire", "task-99", "fs_write")

	entries, err = os.ReadDir(taskDir)
	if err != nil {
		t.Fatalf("ReadDir after purge: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("undo/task-99 has %d entries after purge, want 0 (empty)", len(entries))
	}

	// A purged trace's undo record is gone: undo must now fail cleanly.
	if err := s.UndoWrite(context.Background(), "trace-expire"); !errors.Is(err, ErrNoUndoRecord) {
		t.Errorf("UndoWrite after purge = %v, want ErrNoUndoRecord", err)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
}
