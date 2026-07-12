package consolidation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
)

// runGit mirrors kahyad/internal/backup/gitpush_test.go's OWN identical
// helper (test-fixture setup only - shells out to the real `git` binary
// against a throwaway temp directory, never the code path under test).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initKahyaRepo creates a fresh ~/Kahya-shaped repo (main branch, one
// seed commit) in a t.TempDir() - the same "temp git repo, no real
// ~/Kahya" convention every test in this package uses.
func initKahyaRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "seed@example.invalid")
	runGit(t, dir, "config", "user.name", "Seed")
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "memory/note.md", "line one\nline two\nline three\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")
	return dir
}

func writeFile(t *testing.T, repoDir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(repoDir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestComputeUserTouchedLinesNoUserCommitsToday(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()
	touched, err := ComputeUserTouchedLines(context.Background(), git, repo, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if len(touched) != 0 {
		t.Fatalf("touched = %+v, want empty (seed commit is not author=user)", touched)
	}
}

func TestComputeUserTouchedLinesFindsEditedLine(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()

	// A same-day user edit to line 2.
	writeFile(t, repo, "memory/note.md", "line one\nline two EDITED BY USER\nline three\n")
	if err := CommitAll(context.Background(), git, repo, UserCommitAuthor, UserPreCommitMessage); err != nil {
		t.Fatalf("CommitAll() error = %v", err)
	}

	since := time.Now().Add(-1 * time.Hour)
	touched, err := ComputeUserTouchedLines(context.Background(), git, repo, since)
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	lines, ok := touched["memory/note.md"]
	if !ok {
		t.Fatalf("touched = %+v, want an entry for memory/note.md", touched)
	}
	if !lines[2] {
		t.Fatalf("touched[memory/note.md] = %+v, want line 2 marked touched", lines)
	}
}

func TestComputeUserTouchedLinesIgnoresCommitsBeforeSince(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()

	writeFile(t, repo, "memory/note.md", "line one\nline two EDITED\nline three\n")
	if err := CommitAll(context.Background(), git, repo, UserCommitAuthor, UserPreCommitMessage); err != nil {
		t.Fatal(err)
	}

	// "since" set to the far future - this commit happened BEFORE it, so
	// it must not count as "today's" edit.
	future := time.Now().Add(24 * time.Hour)
	touched, err := ComputeUserTouchedLines(context.Background(), git, repo, future)
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if len(touched) != 0 {
		t.Fatalf("touched = %+v, want empty (the only user commit is before `since`)", touched)
	}
}

func TestComputeUserTouchedLinesIgnoresNonUserAuthor(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()

	writeFile(t, repo, "memory/note.md", "line one\nline two BY DAEMON\nline three\n")
	if err := CommitAll(context.Background(), git, repo, KahyaCommitAuthor, "nightly consolidation"); err != nil {
		t.Fatal(err)
	}

	touched, err := ComputeUserTouchedLines(context.Background(), git, repo, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if len(touched) != 0 {
		t.Fatalf("touched = %+v, want empty (a kahyad-authored commit is never user-touched)", touched)
	}
}
