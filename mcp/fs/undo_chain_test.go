// undo_chain_test.go covers the FIX-B-8 review: undo recipes are
// fail-closed against a target that changed since the operation
// (ErrTargetChanged), and one undo call's LIFO chain never reverts an older
// operation underneath a newer one on the same path that could not be
// undone (UndoChain, ErrUndoBlocked). An undo never destroys bytes the
// operation did not produce.
package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestUndoWriteRefusesWhenTargetChangedSinceOp: the file was edited after
// the task's write; undo refuses and leaves both the edit and the pre-image
// copy in place.
func TestUndoWriteRefusesWhenTargetChangedSinceOp(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()
	op := writeOp(t, s, pc, "trace-ch", "task-ch", "~/a.txt", "v1", "tok-1")
	rec, _ := s.registry.Get(op)

	mustWriteFile(t, a, "user-edit")
	_, err := s.UndoOp(ctx, "trace-ch", op)
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("UndoOp after an external edit err = %v, want ErrTargetChanged", err)
	}
	if !strings.Contains(err.Error(), rec.CopyPath) {
		t.Fatalf("refusal %q does not name the kept pre-image %s", err, rec.CopyPath)
	}
	if got := mustReadString(t, a); got != "user-edit" {
		t.Fatalf("a.txt = %q, want the external edit untouched", got)
	}
	if got := mustReadString(t, rec.CopyPath); got != "orig" {
		t.Fatalf("pre-image copy = %q, want orig kept for a manual restore", got)
	}

	// Nothing was consumed by the refusal: once the target holds this
	// operation's bytes again the recipe runs.
	mustWriteFile(t, a, "v1")
	if _, err := s.UndoOp(ctx, "trace-ch", op); err != nil {
		t.Fatalf("UndoOp with the target back at v1: %v", err)
	}
	if got := mustReadString(t, a); got != "orig" {
		t.Fatalf("a.txt = %q, want orig", got)
	}
}

// TestUndoCreatedFileRefusesWhenChangedSinceOp: a file the write CREATED is
// not moved to Trash once someone else changed it.
func TestUndoCreatedFileRefusesWhenChangedSinceOp(t *testing.T) {
	home := testHome(t)
	n := filepath.Join(home, "new.txt")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	op := writeOp(t, s, pc, "trace-cn", "task-cn", "~/new.txt", "task-content", "tok-1")

	mustWriteFile(t, n, "user-content")
	if _, err := s.UndoOp(context.Background(), "trace-cn", op); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("UndoOp err = %v, want ErrTargetChanged", err)
	}
	if got := mustReadString(t, n); got != "user-content" {
		t.Fatalf("new.txt = %q, want the user's bytes in place", got)
	}
	if entries, _ := os.ReadDir(s.TrashDir); len(entries) != 0 {
		t.Fatalf("Trash holds %d entries, want 0 (nothing moved)", len(entries))
	}
}

// TestUndoWriteRefusesWhenTargetRemovedSinceOp: "absent" is not the state
// the write left either - nothing is re-created behind the user's back.
func TestUndoWriteRefusesWhenTargetRemovedSinceOp(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	op := writeOp(t, s, pc, "trace-rm", "task-rm", "~/a.txt", "v1", "tok-1")

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoOp(context.Background(), "trace-rm", op); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("UndoOp err = %v, want ErrTargetChanged", err)
	}
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatalf("a.txt re-created by a refused undo (lstat err %v)", err)
	}
}

// TestUndoWriteRefusesWhenTargetIsNoLongerARegularFile: a FIFO planted at
// the path is refused without ever blocking on it.
func TestUndoWriteRefusesWhenTargetIsNoLongerARegularFile(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	op := writeOp(t, s, pc, "trace-ff", "task-ff", "~/a.txt", "v1", "tok-1")

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(a, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := s.UndoOp(context.Background(), "trace-ff", op); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("UndoOp err = %v, want ErrTargetChanged", err)
	}
	if info, err := os.Lstat(a); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the FIFO was replaced (lstat %v, %v)", info, err)
	}
}

// TestUndoWriteThenDeleteRecreatedFileIsNeverClobbered is the review
// blocker at this package's level: write (op1) then delete (op2) the same
// file, the file is re-created, then both are undone newest first. op2's
// Trash restore refuses (O_EXCL), and op1 refuses too instead of writing
// its pre-image over the re-created bytes.
func TestUndoWriteThenDeleteRecreatedFileIsNeverClobbered(t *testing.T) {
	home := testHome(t)
	a := filepath.Join(home, "a.txt")
	mustWriteFile(t, a, "orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()
	op1 := writeOp(t, s, pc, "trace-wdr", "task-wdr", "~/a.txt", "task-v1", "tok-1")
	pc.decision = allowDecision("tok-2")
	del, err := s.HandleDelete(ctx, "trace-wdr", "task-wdr", FsDeleteArgs{Path: "~/a.txt"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}

	mustWriteFile(t, a, "user-new-content")
	if _, err := s.UndoOp(ctx, "trace-wdr", del.OpID); !errors.Is(err, ErrTargetChanged) || !strings.Contains(err.Error(), del.TrashPath) {
		t.Fatalf("UndoOp(delete) err = %v, want ErrTargetChanged naming the Trash entry %s", err, del.TrashPath)
	}
	if _, err := s.UndoOp(ctx, "trace-wdr", op1); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("UndoOp(write) err = %v, want ErrTargetChanged", err)
	}
	if got := mustReadString(t, a); got != "user-new-content" {
		t.Fatalf("a.txt = %q, want the re-created bytes untouched", got)
	}
	if got := mustReadString(t, del.TrashPath); got != "task-v1" {
		t.Fatalf("Trash entry = %q, want the deleted bytes kept", got)
	}
}

// TestUndoChainBlocksOlderSamePathOpsAfterNewerFailure: within one chain,
// once the newer op on a path fails, the older op on that path is not
// attempted - even though the file was re-created with exactly the bytes
// the older op wrote, so its own post-state check would pass. An older op
// on another path still runs, and a no_record op poisons nothing.
func TestUndoChainBlocksOlderSamePathOpsAfterNewerFailure(t *testing.T) {
	home := testHome(t)
	a, b := filepath.Join(home, "a.txt"), filepath.Join(home, "b.txt")
	mustWriteFile(t, a, "orig")
	mustWriteFile(t, b, "B-orig")
	pc := &fakePolicyClient{}
	s := newTestServer(t, home, nil, nil, pc, &fakeLedger{})
	ctx := context.Background()
	const trace = "trace-chain"
	op0 := writeOp(t, s, pc, trace, "task-chain", "~/b.txt", "B-new", "tok-0")
	op1 := writeOp(t, s, pc, trace, "task-chain", "~/a.txt", "task-v1", "tok-1")
	pc.decision = allowDecision("tok-2")
	del, err := s.HandleDelete(ctx, trace, "task-chain", FsDeleteArgs{Path: "~/a.txt"})
	if err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}
	mustWriteFile(t, a, "task-v1") // re-created with the very bytes op1 wrote

	chain := s.NewUndoChain()
	if _, err := chain.UndoOp(ctx, trace, normalizeOpID("")); !errors.Is(err, ErrNoUndoRecord) {
		t.Fatalf("chain.UndoOp(unknown op) err = %v, want ErrNoUndoRecord", err)
	}
	if _, err := chain.UndoOp(ctx, trace, del.OpID); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("chain.UndoOp(delete) err = %v, want ErrTargetChanged", err)
	}
	res, err := chain.UndoOp(ctx, trace, op1)
	if !errors.Is(err, ErrUndoBlocked) || res.CanonicalPath == "" || res.Tool != "fs_write" {
		t.Fatalf("chain.UndoOp(older same-path write) = %+v, %v, want ErrUndoBlocked with its path", res, err)
	}
	if got := mustReadString(t, a); got != "task-v1" {
		t.Fatalf("a.txt = %q, want the re-created bytes untouched", got)
	}
	if _, ok := s.registry.Get(op1); !ok {
		t.Fatal("the blocked op's record was dropped; nothing ran, it must stay")
	}
	if _, err := chain.UndoOp(ctx, trace, op0); err != nil {
		t.Fatalf("chain.UndoOp(other path): %v", err)
	}
	if got := mustReadString(t, b); got != "B-orig" {
		t.Fatalf("b.txt = %q, want B-orig", got)
	}
}
