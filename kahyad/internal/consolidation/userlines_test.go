package consolidation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// --- FIX-P-2: Turkish (non-ASCII) and glob-character note names ---

// userCommit commits every change in repo as author=user (a same-day user
// edit made before the nightly run).
func userCommit(t *testing.T, repo string) {
	t.Helper()
	if err := CommitAll(context.Background(), backup.NewExecGitRunner(), repo, UserCommitAuthor, UserPreCommitMessage); err != nil {
		t.Fatalf("user commit: %v", err)
	}
}

func sortedLines(m map[int]bool) []int {
	var out []int
	for l, ok := range m {
		if ok {
			out = append(out, l)
		}
	}
	sort.Ints(out)
	return out
}

func TestComputeUserTouchedLinesTurkishFilename(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/günlük.md", "a\nb\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	writeFile(t, repo, "memory/günlük.md", "a\nUSER\n")
	userCommit(t, repo)

	got, err := ComputeUserTouchedLines(context.Background(), backup.NewExecGitRunner(), repo, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if lines := sortedLines(got["memory/günlük.md"]); len(lines) != 1 || lines[0] != 2 {
		t.Fatalf("touched[memory/günlük.md] = %v (whole map %v), want [2]", lines, got)
	}
}

// TestComputeUserTouchedLinesNFDOnDiskKeyIsNFC: a note whose name is
// written in decomposed (NFD) form is keyed by its NFC form, whatever form
// git reports (NFC via core.precomposeunicode on macOS, raw NFD bytes on
// Linux), and a lookup through NormalizePathKey of either form finds it.
func TestComputeUserTouchedLinesNFDOnDiskKeyIsNFC(t *testing.T) {
	const nfd = "memory/gu\u0308nlu\u0308k.md"
	const nfc = "memory/g\u00fcnl\u00fck.md"
	if nfd == nfc {
		t.Fatal("fixture: the NFD and NFC spellings must differ byte-wise")
	}
	repo := initKahyaRepo(t)
	writeFile(t, repo, nfd, "a\nb\nc\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	writeFile(t, repo, nfd, "a\nb\nUSER\n")
	userCommit(t, repo)

	got, err := ComputeUserTouchedLines(context.Background(), backup.NewExecGitRunner(), repo, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if NormalizePathKey(nfd) != nfc || NormalizePathKey(nfc) != nfc {
		t.Fatalf("NormalizePathKey(nfd)=%q NormalizePathKey(nfc)=%q, want both %q", NormalizePathKey(nfd), NormalizePathKey(nfc), nfc)
	}
	if _, ok := got[nfd]; ok {
		t.Errorf("result keyed by the NFD spelling %q; keys must be NFC", nfd)
	}
	for _, lookup := range []string{nfd, nfc} {
		if lines := sortedLines(got[NormalizePathKey(lookup)]); len(lines) != 1 || lines[0] != 3 {
			t.Errorf("touched[NormalizePathKey(%q)] = %v (whole map %v), want [3]", lookup, lines, got)
		}
	}
}

// TestComputeUserTouchedLinesGlobCharsInFilename (FIX-P-2-E1): a note named
// "n[ab].md" is a glob pathspec to git; without ":(literal)" its per-file
// diff also pulls in na.md's hunk, so na.md's edited line would be marked
// as touched in n[ab].md.
func TestComputeUserTouchedLinesGlobCharsInFilename(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/n[ab].md", "a\nb\nc\n")
	writeFile(t, repo, "memory/na.md", "x\ny\nz\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	writeFile(t, repo, "memory/n[ab].md", "a\nUSER B\nc\n")
	writeFile(t, repo, "memory/na.md", "x\ny\nUSER Z\n")
	userCommit(t, repo)

	got, err := ComputeUserTouchedLines(context.Background(), backup.NewExecGitRunner(), repo, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ComputeUserTouchedLines() error = %v", err)
	}
	if lines := sortedLines(got["memory/n[ab].md"]); len(lines) != 1 || lines[0] != 2 {
		t.Errorf("touched[memory/n[ab].md] = %v, want [2] only (never na.md's line 3)", lines)
	}
	if lines := sortedLines(got["memory/na.md"]); len(lines) != 1 || lines[0] != 3 {
		t.Errorf("touched[memory/na.md] = %v, want [3]", lines)
	}
}

func TestSplitNULDropsEmpty(t *testing.T) {
	if got := splitNUL("a\x00b\x00"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitNUL(a NUL b NUL) = %q, want [a b]", got)
	}
	if got := splitNUL("a\x00\x00b"); len(got) != 2 {
		t.Errorf("splitNUL(a NUL NUL b) = %q, want [a b]", got)
	}
	if got := splitNUL(""); got != nil {
		t.Errorf("splitNUL(\"\") = %q, want nil", got)
	}
	if got := splitNUL("with space\nand newline\x00"); len(got) != 1 || got[0] != "with space\nand newline" {
		t.Errorf("splitNUL kept/split a name wrongly: %q", got)
	}
}
