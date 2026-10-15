package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLockRepoBlocksMemoryWrite (FIX-FINAL precommit-claims-kahyad-
// written-note-as-user): LockRepo is the same mutex HandleWrite holds
// across its write-then-commit sequence, so while an out-of-package git
// mutation (consolidation's pre-commit) holds it, memory_write does not
// even write the file.
func TestLockRepoBlocksMemoryWrite(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	unlock := s.LockRepo()
	done := make(chan error, 1)
	go func() {
		_, err := s.HandleWrite(context.Background(), "tid-lock", MemoryWriteArgs{Content: "not", File: "inbox/kilit.md"})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(memDir, "inbox", "kilit.md")); err == nil {
		t.Fatal("memory_write wrote its file while the repo lock was held")
	}
	select {
	case err := <-done:
		t.Fatalf("HandleWrite returned (%v) while the repo lock was held", err)
	default:
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HandleWrite after unlock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HandleWrite did not proceed after unlock")
	}
}
