package consolidation

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
)

// chanRepoLock is a RepoLocker whose lock is "held by a memory tool" until
// release is closed; entered fires when consolidation asks for it.
type chanRepoLock struct {
	entered chan struct{}
	release chan struct{}
}

func (l *chanRepoLock) LockMemoryRepo() func() {
	select {
	case l.entered <- struct{}{}:
	default:
	}
	<-l.release
	return func() {}
}

func lastMemoryAuthor(t *testing.T, repo, rel string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%an <%ae>", "--", rel).CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPreCommitWaitsForMemoryRepoLock (FIX-FINAL precommit-claims-kahyad-
// written-note-as-user): memory_write / ingest write a note BEFORE they
// commit it as kahyad. Consolidation's author=user pre-commit did not take
// the memory tools' repo mutex, so a run landing in that window committed
// kahyad's agent_derived note as the user's own edit (user_edit, the top
// tier). The pre-commit now holds RepoLock: while a memory tool holds it,
// no "user" commit happens.
func TestPreCommitWaitsForMemoryRepoLock(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/rutin.md", "# Rutin\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	// a memory tool's write that is not committed yet
	writeFile(t, repo, "memory/inbox/2026-10-15.md", "---\nkahya_source_tier: agent_derived\n---\n- not\n")

	lock := &chanRepoLock{entered: make(chan struct{}, 1), release: make(chan struct{})}
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:      Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:      backup.NewExecGitRunner(),
		RepoLock: lock,
		Cloud: SessionFunc(func(context.Context, string, map[string]string) (map[string]string, error) {
			return map[string]string{}, nil
		}),
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   &fakeReindexer{},
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), "trace-lock") }()

	select {
	case <-lock.entered:
	case err := <-done:
		t.Fatalf("Run finished (%v) without ever asking for the memory-repo lock", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run never asked for the memory-repo lock")
	}
	time.Sleep(100 * time.Millisecond)
	if got := lastMemoryAuthor(t, repo, "memory/inbox/2026-10-15.md"); got != "" {
		t.Fatalf("the uncommitted note was committed (%q) while the memory tool still held the repo lock", got)
	}
	close(lock.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not finish after the lock was released")
	}
}
