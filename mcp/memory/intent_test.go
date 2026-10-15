package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crashAfterWrite reproduces the on-disk state when kahyad dies between a
// memory write and its commit: the intent is journalled and the bytes are
// on disk, nothing is committed.
func crashAfterWrite(t *testing.T, s *Server, repoRoot, rel string, content []byte) string {
	t.Helper()
	abs := filepath.Join(s.MemoryDir, rel)
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	resolvedAbs := filepath.Join(resolvedRoot, "memory", rel)
	if _, err := s.recordWriteIntent(resolvedRoot, resolvedAbs, "memory_write", "trace-crashed", content); err != nil {
		t.Fatalf("recordWriteIntent: %v", err)
	}
	if err := writeComposedMemoryFile(abs, content); err != nil {
		t.Fatal(err)
	}
	return abs
}

func pendingIntents(t *testing.T, repoRoot string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, ".git", pendingDirName))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestRecoverPendingWritesCommitsOrphanAsKahyad (FIX-FINAL crash-orphan-
// note-precommitted-as-user-edit): a note kahyad wrote but never committed
// is committed as author kahyad, reindexed and ledgered, and its intent is
// removed.
func TestRecoverPendingWritesCommitsOrphanAsKahyad(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	idx := &fakeIndexer{episodeID: 7}
	led := &fakeLedger{}
	s := newTestServer(memDir, nil, idx, led)
	crashAfterWrite(t, s, repoRoot, "inbox/2026-10-15.md", []byte("---\nkahya_source_tier: agent_derived\n---\nnot\n"))

	if err := s.RecoverPendingWrites(context.Background(), "trace-boot"); err != nil {
		t.Fatalf("RecoverPendingWrites: %v", err)
	}
	if author := runGitT(t, repoRoot, "log", "-1", "--format=%an", "--", "memory/inbox/2026-10-15.md"); author != "kahyad" {
		t.Fatalf("recovered note's author = %q, want kahyad", author)
	}
	if st := runGitT(t, repoRoot, "status", "--porcelain"); st != "" {
		t.Fatalf("tree not clean after recovery: %q", st)
	}
	if len(idx.calls) != 1 || idx.calls[0] != "inbox/2026-10-15.md" {
		t.Errorf("reindex calls = %v, want the recovered note", idx.calls)
	}
	if kinds := eventKinds(led); len(kinds) != 1 || kinds[0] != EventMemoryWriteRecovered {
		t.Errorf("ledger = %v, want one %s", kinds, EventMemoryWriteRecovered)
	}
	if n := pendingIntents(t, repoRoot); n != 0 {
		t.Errorf("pending intents after recovery = %d, want 0", n)
	}
}

// TestRecoverPendingWritesLeavesUserEditedNote: bytes changed after the
// crash are the user's edit - the intent is dropped and the file stays
// dirty for the nightly author=user pre-commit.
func TestRecoverPendingWritesLeavesUserEditedNote(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{}, &fakeLedger{})
	abs := crashAfterWrite(t, s, repoRoot, "inbox/a.md", []byte("---\nkahya_source_tier: agent_derived\n---\nnot\n"))
	if err := os.WriteFile(abs, []byte("# kullanıcının kendi notu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverPendingWrites(context.Background(), "trace-boot"); err != nil {
		t.Fatalf("RecoverPendingWrites: %v", err)
	}
	if st := runGitT(t, repoRoot, "status", "--porcelain"); !strings.Contains(st, "memory/") {
		t.Fatalf("the user-edited note was committed by recovery (status %q)", st)
	}
	if n := pendingIntents(t, repoRoot); n != 0 {
		t.Errorf("pending intents = %d, want the stale one dropped", n)
	}
}

// TestRecoverPendingWritesKeepsIntentWhenCommitFails: a failing recovery
// commit returns an error and keeps the intent (fail-closed: the caller
// must not commit the file as the user).
func TestRecoverPendingWritesKeepsIntentWhenCommitFails(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{}, &fakeLedger{})
	crashAfterWrite(t, s, repoRoot, "inbox/a.md", []byte("---\nkahya_source_tier: agent_derived\n---\nnot\n"))
	installFailingPreCommitHook(t, repoRoot)
	if err := s.RecoverPendingWrites(context.Background(), "trace-boot"); err == nil {
		t.Fatal("RecoverPendingWrites with a failing commit returned nil")
	}
	if n := pendingIntents(t, repoRoot); n != 1 {
		t.Errorf("pending intents = %d, want the intent kept", n)
	}
}

// TestMemoryWriteClearsIntentOnSuccessAndFailure: an ordinary memory_write
// leaves no intent behind, committed or rolled back.
func TestMemoryWriteClearsIntentOnSuccessAndFailure(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleWrite(context.Background(), "tr", MemoryWriteArgs{File: "inbox/a.md", Content: "bir"}); err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if n := pendingIntents(t, repoRoot); n != 0 {
		t.Fatalf("pending intents after a committed write = %d, want 0", n)
	}
	installFailingPreCommitHook(t, repoRoot)
	if _, err := s.HandleWrite(context.Background(), "tr", MemoryWriteArgs{File: "inbox/b.md", Content: "iki"}); err == nil {
		t.Fatal("HandleWrite with a failing commit returned nil")
	}
	if n := pendingIntents(t, repoRoot); n != 0 {
		t.Fatalf("pending intents after a rolled-back write = %d, want 0", n)
	}
}
